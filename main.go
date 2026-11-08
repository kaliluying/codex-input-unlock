package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
)

var appVersion = "1.0.0"

// ============================================================================
// 1. 配额门禁数据结构与核心模式匹配引擎 (Quota Gate Models & Matching)
// ============================================================================

// Location 描述前端 JS Bundle 中锁定位点的具体物理坐标与上下文变量名
type Location struct {
	URLRegex                  string `json:"urlRegex"`                            // 匹配的前端脚本 URL 正则表达式
	LineNumber                int    `json:"lineNumber"`                          // 目标断点行号 (0-based)
	ColumnNumber              int    `json:"columnNumber"`                        // 目标断点列号 (以 UTF-16 Code Unit 计量)
	QuotaVariable             string `json:"quotaVariable"`                       // 前端代码中负责额度封锁判断的关键变量名
	HostVariable              string `json:"hostVariable,omitempty"`              // 宿主/环境上下文变量名（可选）
	ConversationQuotaVariable string `json:"conversationQuotaVariable,omitempty"` // 会话级额外额度控制变量（针对双变量校验模式）
	Mode                      string `json:"mode"`                                // 识别并适用的定位模式标识符
	EndLineNumber             *int   `json:"endLineNumber,omitempty"`             // 作用域结束行号（可选）
	EndColumnNumber           *int   `json:"endColumnNumber,omitempty"`           // 作用域结束列号（可选）
}

// MatchedBreakpoint 包含经定位后最终生成的可直接下发给 CDP 的断点信息
type MatchedBreakpoint struct {
	Location  Location `json:"location"`  // 断点坐标
	Condition string   `json:"condition"` // 注入执行的 JavaScript 条件表达式
}

// ExplainReport 用于自测与诊断输出的规则匹配命中报告
type ExplainReport struct {
	AtomCount      int      `json:"atomCount"`      // 检测到的 Quota Atom 数量
	SubmitDisabled bool     `json:"submitDisabled"` // 是否检测到按钮禁用状态
	Matched        []string `json:"matched"`        // 命中的匹配规则列表
}

// JS 标识符正则片段（支持字母、$、_ 开头，后接数字字母）
const ident = `[A-Za-z_$][a-zA-Z0-9_$]*`

// 安全校验正则：防止变量名中包含恶意注入的特殊符号
var safeIdentRe = regexp.MustCompile(`^[A-Za-z_$][a-zA-Z0-9_$]*$`)

// utf16Len 计算字符串以 UTF-16 Code Units 计量的长度
// 【核心原因】：Chrome DevTools Protocol (CDP) 规定所有的 ColumnNumber 必须以 UTF-16
// 码元为单位计算。如果直接使用 Go 的 byte 或 rune 计数，当源码中包含 Emoji 或中文等多字节字符时，
// 断点列偏移量就会产生偏差，导致注入失败或错位。
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sourceLocation 根据 JS 源码全文和绝对字符偏移量，换算出 0-based 的行号与 UTF-16 列号
func sourceLocation(source, sourceURL string, absoluteOffset int, quotaVar, hostVar, mode string, convQuotaVar string, endOffset *int) Location {
	preceding := source[:absoluteOffset]
	lines := strings.Split(preceding, "\n")
	lineNum := len(lines) - 1
	colNum := utf16Len(lines[len(lines)-1])

	loc := Location{
		URLRegex:                  "^" + regexp.QuoteMeta(sourceURL) + "$",
		LineNumber:                lineNum,
		ColumnNumber:              colNum,
		QuotaVariable:             quotaVar,
		HostVariable:              hostVar,
		ConversationQuotaVariable: convQuotaVar,
		Mode:                      mode,
	}

	if endOffset != nil && *endOffset > absoluteOffset && *endOffset <= len(source) {
		endPreceding := source[:*endOffset]
		endLines := strings.Split(endPreceding, "\n")
		endLineNum := len(endLines) - 1
		endColNum := utf16Len(endLines[len(endLines)-1])
		loc.EndLineNumber = &endLineNum
		loc.EndColumnNumber = &endColNum
	}

	return loc
}

// conditionForLocation 为目标断点生成执行改写的 JS 条件表达式
// 【原理精妙】：
// 1. 我们利用 CDP 的 "条件断点" (Breakpoint Condition) 能力向当前 JS 执行栈注入代码。
// 2. 利用 JavaScript 的“逗号操作符”特性：`(` + 表达式1 + `,` + 表达式2 + `)`。
//    例如 `(quotaVar={}, false)`：
//    - 前半部分 `quotaVar={}` 或 `quotaVar=false` 会在变量使用前将其改写为未封锁状态；
//    - 最后一个操作数恒返回 `false`。
// 3. 【为什么恒返回 false】：
//    如果返回 true，浏览器会真正在该行停住暂停（Pause），导致整个应用界面卡死等待调试器。
//    而返回 `false` 则会让 V8 引擎在“执行完修改副作用”后立即继续执行，完全不阻塞 UI，
//    达到真正的“静默热重写内存变量”，普通用户在使用时完全无感知！
func conditionForLocation(loc *Location) string {
	if loc == nil || !safeIdentRe.MatchString(loc.QuotaVariable) {
		return ""
	}
	quota := loc.QuotaVariable
	switch loc.Mode {
	case "codex-local-quota", "legacy-local-host", "local-submit-quota", "composer-quota-consumer":
		return fmt.Sprintf("(%s=false,false)", quota)
	case "codex-rate-limit-state":
		return fmt.Sprintf("(%s=null,false)", quota)
	case "codex-rate-limit-derived", "codex-rate-limit-derived-v2":
		// 最新版 ChatGPT/Codex 中，当处于封锁状态时变量为具有限额详情的对象，
		// 将其重写为空对象 `{}` 即可避开 `.block_reason` 与 `.resets_at` 等门禁判断
		return fmt.Sprintf("(%s={},false)", quota)
	case "quota-atom-data":
		return fmt.Sprintf("(__codexExternalQuotaGate.unlock(%s),false)", quota)
	case "third-party-composer-dual":
		if !safeIdentRe.MatchString(loc.ConversationQuotaVariable) {
			return ""
		}
		return fmt.Sprintf("(%s=false,%s=false,false)", loc.ConversationQuotaVariable, quota)
	}
	return ""
}

// locateRateLimitState 定位 Codex 较早版本中的 sendBlockReason 额度封锁逻辑
// 模式特征：sendBlockReason: <var>?.block_reason ?? null, sendBlocked: <var> != null
func locateRateLimitState(source, sourceURL string) *Location {
	pattern := `sendBlockReason:(` + ident + `)\?\.block_reason\?\?null,sendBlocked:(` + ident + `)!=null`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatchIndex(source, -1)
	var matched [][]int
	for _, m := range matches {
		q1 := source[m[2]:m[3]]
		q2 := source[m[4]:m[5]]
		// 确保前后两个判断使用的是同一个上下文变量
		if q1 == q2 {
			matched = append(matched, m)
		}
	}
	// 严格要求唯一匹配，避免多处误伤
	if len(matched) != 1 {
		return nil
	}
	m := matched[0]
	quota := source[m[2]:m[3]]
	absoluteOffset := m[0] + len("sendBlockReason:")
	loc := sourceLocation(source, sourceURL, absoluteOffset, quota, "", "codex-rate-limit-state", "", nil)
	return &loc
}

// locateRateLimitDerivedV2 定位当前最新版 ChatGPT/Codex Electron 前端 Bundle 的派生状态
// 特征：使用 Jotai/Recoil 状态派生函数，检查 authMethod 是否为 "chatgpt"，并提取 rate_limit.allowed
func locateRateLimitDerivedV2(source, sourceURL string) *Location {
	pattern := `(` + ident + `)=\(0,` + ident + `\.un\)\(` + ident + `\.u,\(\{get:` + ident + `\}\)=>\{[^}]{0,800}?if\("chatgpt"!==(` + ident + `)\.authMethod[^}]*?\.rate_limit\?\.allowed!==!1`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatchIndex(source, -1)
	if len(matches) != 1 {
		return nil
	}
	m := matches[0]
	quota := source[m[4]:m[5]]
	matchStr := source[m[0]:m[1]]
	ifIdx := strings.Index(matchStr, `if("chatgpt"!==`)
	if ifIdx == -1 {
		ifIdx = strings.Index(matchStr, "if(")
	}
	if ifIdx == -1 {
		return nil
	}
	absoluteOffset := m[0] + ifIdx + len("if(")
	loc := sourceLocation(source, sourceURL, absoluteOffset, quota, "", "codex-rate-limit-derived-v2", "", nil)
	return &loc
}

// atomPattern 返回查找前端状态管理器中 Quota Atom 定义的核心正则
func atomPattern() *regexp.Regexp {
	return regexp.MustCompile(`(` + ident + `)=` + ident + `\(` + ident + `,\(\{get:` + ident + `\}\)=>\{[^}]*?\.authMethod!==` + "`chatgpt`" + `[^}]*?\??\.rate_limit\??\.allowed!==(!1|false)`)
}

// locateCodexLocalQuota 定位将额度 Atom 绑定到 local host 时的提交控制点
// 适用于识别前端将额度直接注入至 submitDisabled 的链路
func locateCodexLocalQuota(source, sourceURL, atomVariable string) *Location {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(` + ident + `)=` + ident + `\(` + regexp.QuoteMeta(atomVariable) + `\)&&(` + ident + `)===` + "`local`"),
		regexp.MustCompile(`(` + ident + `)=` + ident + `\((` + ident + `)===` + "`local`" + `&&` + regexp.QuoteMeta(atomVariable) + `\)`),
	}
	type cand struct {
		quota         string
		host          string
		absoluteStart int
		absoluteEnd   int
	}
	var candidates []cand

	for _, p := range patterns {
		matches := p.FindAllStringSubmatchIndex(source, -1)
		for _, m := range matches {
			quota := source[m[2]:m[3]]
			host := source[m[4]:m[5]]
			cStart := m[0]
			windowEnd := minInt(len(source), cStart+12000)
			window := source[cStart:windowEnd]

			assignRe := regexp.MustCompile(`let (` + ident + `)=([^,;]{1,900}\|\|` + regexp.QuoteMeta(quota) + `),(` + ident + `);`)
			assignMatches := assignRe.FindAllStringSubmatchIndex(window, -1)
			for _, am := range assignMatches {
				assignedVar := window[am[2]:am[3]]
				nextVar := window[am[6]:am[7]]
				subWindowEnd := minInt(len(window), am[0]+10000)
				if !strings.Contains(window[am[0]:subWindowEnd], "submitDisabled:"+assignedVar) {
					continue
				}
				absStart := cStart + am[0]
				fullStr := window[am[0]:am[1]]
				targetSuffix := "," + nextVar + ";"
				lastIdx := strings.LastIndex(fullStr, targetSuffix)
				if lastIdx == -1 {
					continue
				}
				absEnd := absStart + lastIdx
				candidates = append(candidates, cand{
					quota:         quota,
					host:          host,
					absoluteStart: absStart,
					absoluteEnd:   absEnd,
				})
			}
		}
	}
	if len(candidates) != 1 {
		return nil
	}
	c := candidates[0]
	loc := sourceLocation(source, sourceURL, c.absoluteStart, c.quota, c.host, "codex-local-quota", "", &c.absoluteEnd)
	return &loc
}

func locateLegacy(source, sourceURL, atomVariable string) *Location {
	pattern := regexp.MustCompile(`(` + ident + `)=` + ident + `\(` + regexp.QuoteMeta(atomVariable) + `\)&&(` + ident + `)===` + "`local`")
	matches := pattern.FindAllStringSubmatchIndex(source, -1)
	if len(matches) != 1 {
		return nil
	}
	m := matches[0]
	quota := source[m[2]:m[3]]
	host := source[m[4]:m[5]]
	tailOffset := m[1]
	tailEnd := minInt(len(source), tailOffset+18000)
	tail := source[tailOffset:tailEnd]

	assignPattern := regexp.MustCompile(`(` + ident + `)=(` + ident + `(?:\|\|` + ident + `)*\|\|` + regexp.QuoteMeta(quota) + `)([,;])`)
	assignMatch := assignPattern.FindStringSubmatchIndex(tail)
	if assignMatch == nil {
		return nil
	}
	assignedVar := tail[assignMatch[2]:assignMatch[3]]
	if !strings.Contains(tail, "submitDisabled:"+assignedVar) {
		return nil
	}
	absOffset := tailOffset + assignMatch[0] + (assignMatch[3] - assignMatch[2]) + 1
	loc := sourceLocation(source, sourceURL, absOffset, quota, host, "legacy-local-host", "", nil)
	return &loc
}

func locateModern(source, sourceURL, atomVariable string) *Location {
	pattern := regexp.MustCompile(`(` + ident + `)=` + ident + `\(` + regexp.QuoteMeta(atomVariable) + `\)&&(` + ident + `)`)
	matches := pattern.FindAllStringSubmatchIndex(source, -1)
	type cand struct {
		quota        string
		offset       int
		convQuotaVar string
	}
	var candidates []cand
	for _, m := range matches {
		quota := source[m[2]:m[3]]
		wEnd := minInt(len(source), m[0]+60000)
		window := source[m[0]:wEnd]
		if !strings.Contains(window, "submitDisabled:") || !strings.Contains(window, "rateLimitSendBlocked:") {
			continue
		}
		propsPattern := regexp.MustCompile(`rateLimitSendBlocked:(` + ident + `)\|\|` + regexp.QuoteMeta(quota) + `,rateLimitConversationSendBlocked:(` + ident + `)[,}]`)
		pMatch := propsPattern.FindStringSubmatchIndex(window)
		if pMatch == nil {
			continue
		}
		p1 := window[pMatch[2]:pMatch[3]]
		p2 := window[pMatch[4]:pMatch[5]]
		if p1 != p2 {
			continue
		}
		candidates = append(candidates, cand{
			quota:        quota,
			offset:       m[0] + pMatch[0],
			convQuotaVar: p1,
		})
	}
	if len(candidates) != 1 {
		return nil
	}
	c := candidates[0]
	loc := sourceLocation(source, sourceURL, c.offset, c.quota, "", "third-party-composer-dual", c.convQuotaVar, nil)
	return &loc
}

func locateRateLimitDerived(source, sourceURL string, atomIndex int) *Location {
	tailEnd := minInt(len(source), atomIndex+4000)
	tail := source[atomIndex:tailEnd]
	guardPattern := regexp.MustCompile(`;if\((` + ident + `)\.authMethod!==` + "`chatgpt`")
	m := guardPattern.FindStringSubmatchIndex(tail)
	if m == nil {
		return nil
	}
	quota := tail[m[2]:m[3]]
	absOffset := atomIndex + m[0] + len(";if(")
	loc := sourceLocation(source, sourceURL, absOffset, quota, "", "codex-rate-limit-derived", "", nil)
	return &loc
}

func locateComposerQuotaConsumer(source, sourceURL, atomVariable string) *Location {
	readPattern := regexp.MustCompile(`(` + ident + `)=` + ident + `\(` + ident + `\?` + regexp.QuoteMeta(atomVariable) + `:(?:!1|false)\)`)
	reads := readPattern.FindAllStringSubmatchIndex(source, -1)
	if len(reads) == 0 {
		return nil
	}
	type cand struct {
		absStart    int
		quota       string
		quotaOffset int
	}
	candidates := make(map[int]cand)
	for _, r := range reads {
		quota := source[r[2]:r[3]]
		tailStart := r[0]
		tailEnd := minInt(len(source), tailStart+60000)
		tail := source[tailStart:tailEnd]
		consumerPattern := regexp.MustCompile(`(` + ident + `)=([^,;]*?)\|\|` + regexp.QuoteMeta(quota) + `\|\|`)
		cMatches := consumerPattern.FindAllStringSubmatchIndex(tail, -1)
		for _, cm := range cMatches {
			absStart := tailStart + cm[0]
			if _, exists := candidates[absStart]; exists {
				continue
			}
			wEnd := minInt(len(source), absStart+40000)
			window := source[absStart:wEnd]
			if !strings.Contains(window, "submitDisabled:") {
				continue
			}
			v1Len := cm[3] - cm[2]
			v2Len := cm[5] - cm[4]
			quotaOffset := absStart + v1Len + 1 + v2Len + 2
			candidates[absStart] = cand{
				absStart:    absStart,
				quota:       quota,
				quotaOffset: quotaOffset,
			}
		}
	}
	if len(candidates) != 1 {
		return nil
	}
	var c cand
	for _, v := range candidates {
		c = v
	}
	loc := sourceLocation(source, sourceURL, c.absStart, c.quota, "", "composer-quota-consumer", "", &c.quotaOffset)
	return &loc
}

func locateQuotaAtomGuard(source, sourceURL, atomVariable string) *Location {
	guardPattern := regexp.MustCompile(regexp.QuoteMeta(atomVariable) + `=` + ident + `\(` + ident + `,\(\{get:` + ident + `\}\)=>\{([^}]*?)(` + ident + `)\??\.rate_limit\??\.allowed!==(!1|false)`)
	m := guardPattern.FindStringSubmatchIndex(source)
	if m == nil {
		return nil
	}
	dataVar := source[m[4]:m[5]]
	quotaReadOffset := m[4]
	guardStr := source[m[0]:m[1]]
	arrow := strings.Index(guardStr, "=>")
	if arrow < 0 {
		return nil
	}
	braceRel := strings.Index(guardStr[arrow:], "{")
	if braceRel < 0 {
		return nil
	}
	brace := arrow + braceRel
	bodyStart := m[0] + brace + 1
	if bodyStart >= quotaReadOffset {
		return nil
	}
	before := source[bodyStart:quotaReadOffset]
	ifRe := regexp.MustCompile(`\bif\s*\(`)
	ifMatches := ifRe.FindAllStringIndex(before, -1)
	ifIndex := -1
	for _, im := range ifMatches {
		abs := bodyStart + im[0]
		if strings.Contains(source[abs:quotaReadOffset], ".authMethod!==") {
			ifIndex = abs
			break
		}
	}
	if ifIndex < 0 {
		return nil
	}
	loc := sourceLocation(source, sourceURL, ifIndex, dataVar, "", "quota-atom-data", "", &quotaReadOffset)
	return &loc
}

func locateLocalSubmit(source, sourceURL, atomVariable string) *Location {
	consumerPattern := regexp.MustCompile(`(` + ident + `)=` + ident + `\((?:[^()]{0,200}?&&\s*)?` + regexp.QuoteMeta(atomVariable) + `\)`)
	matches := consumerPattern.FindAllStringSubmatchIndex(source, -1)
	type cand struct {
		quota string
		start int
	}
	var candidates []cand
	for _, m := range matches {
		quota := source[m[2]:m[3]]
		tailEnd := minInt(len(source), m[0]+20000)
		tail := source[m[0]:tailEnd]
		blockPattern := regexp.MustCompile(`(` + ident + `)=[^;]*?\|\|` + regexp.QuoteMeta(quota) + `([,;)|])`)
		bMatches := blockPattern.FindAllStringSubmatchIndex(tail, -1)
		for _, bm := range bMatches {
			blockVar := tail[bm[2]:bm[3]]
			absStart := m[0] + bm[0]
			wEnd := minInt(len(source), absStart+12000)
			window := source[absStart:wEnd]
			if !strings.Contains(window, "submitDisabled:"+blockVar) {
				continue
			}
			eqIdx := strings.Index(source[absStart:], "=")
			if eqIdx < 0 {
				continue
			}
			start := absStart + eqIdx + 1
			for start < len(source) && (source[start] == ' ' || source[start] == '\t' || source[start] == '\r' || source[start] == '\n') {
				start++
			}
			candidates = append(candidates, cand{quota: quota, start: start})
		}
	}
	if len(candidates) != 1 {
		return nil
	}
	c := candidates[0]
	end := c.start + 1
	loc := sourceLocation(source, sourceURL, c.start, c.quota, "", "local-submit-quota", "", &end)
	return &loc
}

func locateAll(source, sourceURL string) []MatchedBreakpoint {
	if len(source) == 0 || len(sourceURL) == 0 {
		return nil
	}
	if loc := locateRateLimitState(source, sourceURL); loc != nil {
		cond := conditionForLocation(loc)
		if cond != "" {
			return []MatchedBreakpoint{{Location: *loc, Condition: cond}}
		}
	}
	if loc := locateRateLimitDerivedV2(source, sourceURL); loc != nil {
		cond := conditionForLocation(loc)
		if cond != "" {
			return []MatchedBreakpoint{{Location: *loc, Condition: cond}}
		}
	}

	atomRe := atomPattern()
	atomMatches := atomRe.FindAllStringSubmatchIndex(source, -1)
	if len(atomMatches) != 1 {
		return nil
	}
	atomVariable := source[atomMatches[0][2]:atomMatches[0][3]]
	atomIndex := atomMatches[0][0]

	var results []MatchedBreakpoint
	checkLoc := func(loc *Location) {
		if loc == nil {
			return
		}
		cond := conditionForLocation(loc)
		if cond == "" {
			return
		}
		for _, r := range results {
			if r.Location.LineNumber == loc.LineNumber &&
				r.Location.ColumnNumber == loc.ColumnNumber &&
				r.Location.URLRegex == loc.URLRegex {
				return
			}
		}
		results = append(results, MatchedBreakpoint{Location: *loc, Condition: cond})
	}

	checkLoc(locateCodexLocalQuota(source, sourceURL, atomVariable))
	checkLoc(locateLegacy(source, sourceURL, atomVariable))
	checkLoc(locateModern(source, sourceURL, atomVariable))
	checkLoc(locateRateLimitDerived(source, sourceURL, atomIndex))
	checkLoc(locateLocalSubmit(source, sourceURL, atomVariable))
	checkLoc(locateComposerQuotaConsumer(source, sourceURL, atomVariable))
	checkLoc(locateQuotaAtomGuard(source, sourceURL, atomVariable))

	return results
}

func locate(source, sourceURL string) *Location {
	all := locateAll(source, sourceURL)
	if len(all) > 0 {
		return &all[0].Location
	}
	return nil
}

func explain(source, sourceURL string) ExplainReport {
	report := ExplainReport{AtomCount: 0, SubmitDisabled: false, Matched: []string{}}
	if len(source) == 0 {
		return report
	}
	atomMatches := atomPattern().FindAllString(source, -1)
	report.AtomCount = len(atomMatches)
	report.SubmitDisabled = strings.Contains(source, "submitDisabled:")
	for _, m := range locateAll(source, sourceURL) {
		report.Matched = append(report.Matched, m.Location.Mode)
	}
	return report
}

// Built-in JavaScript runtime helper (embedded quota gate fallback).
const embeddedRuntimeGate = `(() => {
  const stats = { unlockCount: 0, lastUnlockOk: null };
  function unlock(data) {
    try {
      const rateLimit = data?.rate_limit;
      if (rateLimit == null || typeof rateLimit !== "object") return false;
      if (rateLimit.allowed === true) {
        stats.unlockCount += 1;
        stats.lastUnlockOk = true;
        return true;
      }
      try { rateLimit.allowed = true; } catch {}
      if (rateLimit.allowed === true) {
        stats.unlockCount += 1;
        stats.lastUnlockOk = true;
        return true;
      }
      try {
        if (Object.isExtensible(data)) data.rate_limit = { ...rateLimit, allowed: true };
      } catch {}
      const ok = data.rate_limit?.allowed === true;
      stats.unlockCount += 1;
      stats.lastUnlockOk = ok;
      return ok;
    } catch {
      stats.unlockCount += 1;
      stats.lastUnlockOk = false;
      return false;
    }
  }
  const api = { stats, unlock };
  globalThis.__codexExternalQuotaGate = api;
})();`

// ============================================================================
// 2. RFC 6455 纯标准库轻量级 WebSocket 客户端 (Zero-Dependency WebSocket)
// ============================================================================
// 【设计背景】：
// 为了实现真正的“单文件零外部依赖”，本项目没有引用 github.com/gorilla/websocket 或
// golang.org/x/net/websocket 等第三方包，而是直接使用 Go 标准库 net/http 和 net 包
// 手工实现了符合 RFC 6455 标准的精简版 WebSocket 客户端。
//
// 该实现完整包含了：
// 1. 标准 HTTP Upgrade 升级握手协议；
// 2. Sec-WebSocket-Key 随机生成与 Sec-WebSocket-Accept 的 SHA-1 校验；
// 3. 客户端向服务端发送数据时的 4 字节掩码（Masking Key）强制异或计算；
// 4. 变长载荷解码（<126, 126=uint16, 127=uint64）；
// 5. 控制帧处理（自动回应 Ping -> Pong，优雅处理 Close 0x8 帧）；
// 6. 线程安全的并发写保护锁。

type wsConn struct {
	conn net.Conn      // 底层 TCP 网络连接
	r    *bufio.Reader // 缓冲读取器
	mu   sync.Mutex    // 并发写入互斥锁，确保同一时刻只有一个协程在写入 WebSocket 数据帧
}

// dialWS 发起与指定 WebSocket 地址的 TCP 连接并完成 RFC 6455 握手流程
func dialWS(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	// 1. 建立基础 TCP 连接
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}

	// 2. 生成 16 字节随机密钥，并按 RFC 规范转为 Base64
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	secKey := base64.StdEncoding.EncodeToString(keyBytes)

	reqPath := u.Path
	if reqPath == "" {
		reqPath = "/"
	}
	if u.RawQuery != "" {
		reqPath += "?" + u.RawQuery
	}

	// 3. 构造标准 WebSocket 升级请求报文
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		reqPath, u.Host, secKey)

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}

	// 4. 读取 HTTP 响应并验证是否成功切换协议（101 Switching Protocols）
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake returned status %d", resp.StatusCode)
	}

	// 5. 校验服务端的 Sec-WebSocket-Accept 签名：
	// 规范算法：SHA-1(Sec-WebSocket-Key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11") 的 Base64
	h := sha1.New()
	h.Write([]byte(secKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expectedAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		conn.Close()
		return nil, errors.New("Sec-WebSocket-Accept mismatch")
	}

	// 握手成功，清空超时定时器，进入长连接消息监听阶段
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, r: reader}, nil
}

// writeText 向 WebSocket 连接发送一条文本帧（Opcode: 0x1）
func (w *wsConn) writeText(msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	payload := []byte(msg)
	length := len(payload)

	var header []byte
	// 首字节：0x81 -> FIN=1 (表示单帧无后续分片), Opcode=0x1 (文本帧)
	header = append(header, 0x81)

	// 次字节及后续长度编码（RFC 6455 规范）：
	// 客户端发送的所有数据帧，MASK 标志位必须置 1（即 | 0x80）
	if length < 126 {
		header = append(header, byte(length|0x80))
	} else if length <= 65535 {
		// 长度在 126 ~ 65535 之间时，后跟 2 字节无符号大端整数
		header = append(header, byte(126|0x80))
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, uint16(length))
		header = append(header, b...)
	} else {
		// 长度大于 65535 时，后跟 8 字节无符号大端整数
		header = append(header, byte(127|0x80))
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(length))
		header = append(header, b...)
	}

	// 生成 4 字节的随机掩码（Masking Key）
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)

	// 对载荷逐字节进行掩码异或混淆处理
	maskedPayload := make([]byte, length)
	for i := 0; i < length; i++ {
		maskedPayload[i] = payload[i] ^ mask[i%4]
	}

	// 一次性写入底层连接
	_, err := w.conn.Write(append(header, maskedPayload...))
	return err
}

// readMessage 从 WebSocket 连接读取下一个完整的应用层文本消息
func (w *wsConn) readMessage() (string, error) {
	for {
		b0, err := w.r.ReadByte()
		if err != nil {
			return "", err
		}
		fin := (b0 & 0x80) != 0
		opcode := b0 & 0x0F

		b1, err := w.r.ReadByte()
		if err != nil {
			return "", err
		}
		hasMask := (b1 & 0x80) != 0
		payloadLen := int(b1 & 0x7F)

		if payloadLen == 126 {
			b := make([]byte, 2)
			if _, err := io.ReadFull(w.r, b); err != nil {
				return "", err
			}
			payloadLen = int(binary.BigEndian.Uint16(b))
		} else if payloadLen == 127 {
			b := make([]byte, 8)
			if _, err := io.ReadFull(w.r, b); err != nil {
				return "", err
			}
			payloadLen = int(binary.BigEndian.Uint64(b))
		}

		var maskKey []byte
		if hasMask {
			maskKey = make([]byte, 4)
			if _, err := io.ReadFull(w.r, maskKey); err != nil {
				return "", err
			}
		}

		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(w.r, payload); err != nil {
			return "", err
		}
		if hasMask {
			for i := 0; i < payloadLen; i++ {
				payload[i] ^= maskKey[i%4]
			}
		}

		switch opcode {
		case 0x1: // text
			return string(payload), nil
		case 0x8: // close
			w.conn.Close()
			return "", io.EOF
		case 0x9: // ping -> reply pong
			pong := []byte{0x8A, 0x80, 0x00, 0x00, 0x00, 0x00}
			w.mu.Lock()
			_, _ = w.conn.Write(pong)
			w.mu.Unlock()
			continue
		case 0xA: // pong
			continue
		default:
			if fin {
				return string(payload), nil
			}
		}
	}
}

func (w *wsConn) close() {
	if w != nil && w.conn != nil {
		_ = w.conn.Close()
	}
}

// ============================================================================
// 3. CDP Session & Supervisor
// ============================================================================

type cdpResponse struct {
	result json.RawMessage
	err    error
}

type cdpMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type CDPSession struct {
	ws       *wsConn
	nextID   int64
	mu       sync.Mutex
	pending  map[int64]chan cdpResponse
	handlers map[string][]func(json.RawMessage)
	closed   chan struct{}
}

func newCDPSession(wsURL string, timeout time.Duration) (*CDPSession, error) {
	ws, err := dialWS(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	s := &CDPSession{
		ws:       ws,
		pending:  make(map[int64]chan cdpResponse),
		handlers: make(map[string][]func(json.RawMessage)),
		closed:   make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

func (s *CDPSession) readLoop() {
	defer func() {
		close(s.closed)
		s.mu.Lock()
		for _, ch := range s.pending {
			ch <- cdpResponse{err: errors.New("cdp session closed")}
		}
		s.pending = make(map[int64]chan cdpResponse)
		s.mu.Unlock()
	}()

	for {
		msgStr, err := s.ws.readMessage()
		if err != nil {
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal([]byte(msgStr), &msg); err != nil {
			continue
		}

		if msg.ID != 0 {
			s.mu.Lock()
			ch, ok := s.pending[msg.ID]
			delete(s.pending, msg.ID)
			s.mu.Unlock()
			if ok {
				if msg.Error != nil {
					ch <- cdpResponse{err: errors.New(msg.Error.Message)}
				} else {
					ch <- cdpResponse{result: msg.Result}
				}
			}
		} else if msg.Method != "" {
			s.mu.Lock()
			listeners := append([]func(json.RawMessage){}, s.handlers[msg.Method]...)
			s.mu.Unlock()
			for _, fn := range listeners {
				go fn(msg.Params)
			}
		}
	}
}

func (s *CDPSession) Send(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&s.nextID, 1)
	req := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ch := make(chan cdpResponse, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.ws.writeText(string(data)); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-time.After(timeout):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("cdp request timed out: %s", method)
	case <-s.closed:
		return nil, errors.New("cdp session closed")
	}
}

func (s *CDPSession) On(event string, handler func(json.RawMessage)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[event] = append(s.handlers[event], handler)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.handlers[event]
		for i, h := range list {
			// Compare address if needed, or clear handlers
			_ = h
			_ = i
		}
	}
}

func (s *CDPSession) Evaluate(expression string) (any, error) {
	raw, err := s.Send("Runtime.evaluate", map[string]any{
		"expression":    expression,
		"awaitPromise":  true,
		"returnByValue": true,
	}, 7*time.Second)
	if err != nil {
		return nil, err
	}
	var res struct {
		ExceptionDetails any `json:"exceptionDetails"`
		Result           struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New("renderer evaluation failed")
	}
	return res.Result.Value, nil
}

func (s *CDPSession) Close() {
	s.ws.close()
}

// discoverTarget 向 Electron 主程序的远程调试端口发起 HTTP 请求，
// 探测当前运行的渲染页面目标（Renderer Page），并锁定 ChatGPT/Codex 的核心窗口。
func discoverTarget(port int) (*cdpTarget, error) {
	fetchTargets := func(endpoint string) ([]cdpTarget, error) {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d", resp.StatusCode)
		}
		var targets []cdpTarget
		if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
			return nil, err
		}
		return targets, nil
	}

	// 优先尝试 IPv4 本地回环地址，若失败则回退尝试 IPv6 [::1]
	targets, err := fetchTargets(fmt.Sprintf("http://127.0.0.1:%d/json", port))
	if err != nil {
		targets, err = fetchTargets(fmt.Sprintf("http://[::1]:%d/json", port))
		if err != nil {
			return nil, err
		}
	}

	type ranked struct {
		target cdpTarget
		rank   int
	}
	var rankedList []ranked
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		r := -1
		// 优先锁定主聊天窗口的渲染页
		if t.URL == "app://-/index.html" {
			r = 100
		} else if strings.HasPrefix(t.URL, "app://") {
			r = 80
		}
		if r >= 0 {
			rankedList = append(rankedList, ranked{target: t, rank: r})
		}
	}
	if len(rankedList) == 0 {
		return nil, errors.New("Codex renderer target not found")
	}
	// 按优先级降序排序，取最优渲染页面
	sort.Slice(rankedList, func(i, j int) bool {
		return rankedList[i].rank > rankedList[j].rank
	})
	res := rankedList[0].target
	return &res, nil
}

// writeJSONAtomic 将结构体以原子文件写入方式持久化到磁盘（先写临时文件再重命名，避免并发脏读）
func writeJSONAtomic(filePath string, value any) error {
	if filePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.%d.tmp", filePath, os.Getpid())
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(temp, data, 0644); err != nil {
		return err
	}
	_ = os.Remove(filePath)
	return os.Rename(temp, filePath)
}

// reportStatus 更新运行状态文件，供 verify 子命令或外部启动脚本查询当前生命周期
func reportStatus(statusPath, state string, extra map[string]any) {
	m := map[string]any{
		"state":     state,
		"pid":       os.Getpid(),
		"updatedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		m[k] = v
	}
	_ = writeJSONAtomic(statusPath, m)
}

func buildBootstrap(loc Location, bundleURL string) string {
	bLoc, _ := json.Marshal(loc)
	bURL, _ := json.Marshal(bundleURL)
	return fmt.Sprintf(`(() => {
    globalThis.__codexInputUnlockState = {
      status: "located",
      bundleUrl: %s,
      breakpoint: %s,
    };
    globalThis.__codexExternalPatchState = globalThis.__codexInputUnlockState;
  })();`, string(bURL), string(bLoc))
}

// installScripts 通过 CDP 的 Page.addScriptToEvaluateOnNewDocument API
// 向页面预先注入辅助运行时（例如兜底的解锁统计与辅助对象）
func installScripts(session *CDPSession, combinedSource string) (string, error) {
	if _, err := session.Send("Runtime.enable", nil, 5*time.Second); err != nil {
		return "", err
	}
	if _, err := session.Send("Page.enable", nil, 5*time.Second); err != nil {
		return "", err
	}

	var installRes struct {
		Identifier string `json:"identifier"`
	}
	raw, err := session.Send("Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source":         combinedSource,
		"runImmediately": true,
	}, 5*time.Second)
	if err != nil {
		raw, err = session.Send("Page.addScriptToEvaluateOnNewDocument", map[string]any{
			"source": combinedSource,
		}, 5*time.Second)
		if err != nil {
			return "", err
		}
	}
	_ = json.Unmarshal(raw, &installRes)

	if _, err := session.Evaluate(combinedSource); err != nil {
		return "", err
	}
	val, err := session.Evaluate("Boolean(globalThis.__codexExternalQuotaGate && (globalThis.__codexInputUnlockState?.breakpoint || globalThis.__codexExternalPatchState?.breakpoint))")
	if err != nil || val != true {
		return "", errors.New("renderer runtime injection did not verify")
	}
	return installRes.Identifier, nil
}

type scriptCandidate struct {
	scriptID    string
	url         string
	breakpoints []MatchedBreakpoint
}

func collectQuotaScripts(session *CDPSession, timeout time.Duration, quietDuration time.Duration) ([]scriptCandidate, error) {
	var mu sync.Mutex
	var found []scriptCandidate
	lastFoundAt := time.Time{}
	parsedScripts := 0
	candidateScripts := 0
	pendingCandidates := int32(0)

	deadline := time.Now().Add(timeout)
	session.On("Debugger.scriptParsed", func(params json.RawMessage) {
		var p struct {
			ScriptID string `json:"scriptId"`
			URL      string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		parsedScripts++
		if p.ScriptID == "" || !strings.HasPrefix(p.URL, "app://") || !strings.HasSuffix(p.URL, ".js") {
			return
		}
		candidateScripts++

		mu.Lock()
		for _, f := range found {
			if f.url == p.URL {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()

		atomic.AddInt32(&pendingCandidates, 1)
		go func() {
			defer atomic.AddInt32(&pendingCandidates, -1)
			srcRaw, err := session.Send("Debugger.getScriptSource", map[string]any{
				"scriptId": p.ScriptID,
			}, 15*time.Second)
			if err != nil {
				return
			}
			var srcRes struct {
				ScriptSource string `json:"scriptSource"`
			}
			if err := json.Unmarshal(srcRaw, &srcRes); err != nil || len(srcRes.ScriptSource) == 0 {
				return
			}

			bps := locateAll(srcRes.ScriptSource, p.URL)
			if len(bps) == 0 {
				return
			}
			if len(bps) > 4 {
				bps = bps[:4]
			}

			mu.Lock()
			defer mu.Unlock()
			for _, f := range found {
				if f.url == p.URL {
					return
				}
			}
			found = append(found, scriptCandidate{
				scriptID:    p.ScriptID,
				url:         p.URL,
				breakpoints: bps,
			})
			lastFoundAt = time.Now()
		}()
	})

	if _, err := session.Send("Debugger.enable", nil, timeout); err != nil {
		return nil, err
	}

	for time.Now().Before(deadline) {
		mu.Lock()
		foundCount := len(found)
		pending := atomic.LoadInt32(&pendingCandidates)
		quiet := !lastFoundAt.IsZero() && time.Since(lastFoundAt) >= quietDuration
		mu.Unlock()

		if foundCount > 0 && pending == 0 && quiet {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(found) == 0 {
		return nil, fmt.Errorf("quota bundle discovery timed out (parsed: %d, candidates: %d)", parsedScripts, candidateScripts)
	}
	return found, nil
}

type cdpLocation struct {
	ScriptID     string `json:"scriptId"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

func resolveBreakpointLocation(session *CDPSession, scriptID string, desired Location) (cdpLocation, error) {
	if desired.EndLineNumber != nil && desired.EndColumnNumber != nil {
		start := cdpLocation{ScriptID: scriptID, LineNumber: desired.LineNumber, ColumnNumber: desired.ColumnNumber}
		end := cdpLocation{ScriptID: scriptID, LineNumber: *desired.EndLineNumber, ColumnNumber: *desired.EndColumnNumber}

		raw, err := session.Send("Debugger.getPossibleBreakpoints", map[string]any{
			"start":               start,
			"end":                 end,
			"restrictToFunction": true,
		}, 5*time.Second)
		if err == nil {
			var res struct {
				Locations []cdpLocation `json:"locations"`
			}
			if err := json.Unmarshal(raw, &res); err == nil && len(res.Locations) > 0 {
				var valid []cdpLocation
				for _, loc := range res.Locations {
					if loc.ScriptID != scriptID {
						continue
					}
					if loc.LineNumber < start.LineNumber || loc.LineNumber > end.LineNumber {
						continue
					}
					if loc.LineNumber == start.LineNumber && loc.ColumnNumber < start.ColumnNumber {
						continue
					}
					if loc.LineNumber == end.LineNumber && loc.ColumnNumber > end.ColumnNumber {
						continue
					}
					valid = append(valid, loc)
				}
				if len(valid) > 0 {
					sort.Slice(valid, func(i, j int) bool {
						if valid[i].LineNumber == valid[j].LineNumber {
							return valid[i].ColumnNumber < valid[j].ColumnNumber
						}
						return valid[i].LineNumber < valid[j].LineNumber
					})
					return valid[0], nil
				}
			}
		}
	}

	for _, radius := range []int{300, 2000} {
		start := cdpLocation{ScriptID: scriptID, LineNumber: desired.LineNumber, ColumnNumber: maxInt(0, desired.ColumnNumber-100)}
		end := cdpLocation{ScriptID: scriptID, LineNumber: desired.LineNumber, ColumnNumber: desired.ColumnNumber + radius}

		raw, err := session.Send("Debugger.getPossibleBreakpoints", map[string]any{
			"start":               start,
			"end":                 end,
			"restrictToFunction": true,
		}, 5*time.Second)
		if err == nil {
			var res struct {
				Locations []cdpLocation `json:"locations"`
			}
			if err := json.Unmarshal(raw, &res); err == nil {
				var valid []cdpLocation
				for _, loc := range res.Locations {
					if loc.ScriptID == scriptID && loc.LineNumber == desired.LineNumber && loc.ColumnNumber >= desired.ColumnNumber {
						valid = append(valid, loc)
					}
				}
				if len(valid) > 0 {
					sort.Slice(valid, func(i, j int) bool {
						return valid[i].ColumnNumber < valid[j].ColumnNumber
					})
					return valid[0], nil
				}
			}
		}
	}

	return cdpLocation{}, fmt.Errorf("no executable breakpoint location at or after %d:%d", desired.LineNumber, desired.ColumnNumber)
}

type armedBreakpoint struct {
	BundleURL         string      `json:"bundleUrl"`
	Mode              string      `json:"mode"`
	Condition         string      `json:"condition"`
	BreakpointID      string      `json:"breakpointId"`
	RequestedLocation cdpLocation `json:"requestedLocation"`
	ActualLocation    cdpLocation `json:"actualLocation"`
}

func superviseTarget(ctx context.Context, target *cdpTarget, gateSource string, port int, statusPath string, onReady func()) error {
	session, err := newCDPSession(target.WebSocketDebuggerURL, 5*time.Second)
	if err != nil {
		return err
	}
	defer session.Close()

	var armedBreakpoints []armedBreakpoint
	var documentScriptID string

	defer func() {
		for _, armed := range armedBreakpoints {
			_, _ = session.Send("Debugger.removeBreakpoint", map[string]any{
				"breakpointId": armed.BreakpointID,
			}, 2*time.Second)
		}
		if documentScriptID != "" {
			_, _ = session.Send("Page.removeScriptToEvaluateOnNewDocument", map[string]any{
				"identifier": documentScriptID,
			}, 2*time.Second)
		}
	}()

	scripts, err := collectQuotaScripts(session, 20*time.Second, 2*time.Second)
	if err != nil {
		return err
	}

	firstBreakpoint := scripts[0].breakpoints[0]
	bootstrap := buildBootstrap(firstBreakpoint.Location, scripts[0].url)
	docID, err := installScripts(session, gateSource+"\n"+bootstrap)
	if err != nil {
		return err
	}
	documentScriptID = docID

	for _, script := range scripts {
		for _, candidate := range script.breakpoints {
			actualLoc, err := resolveBreakpointLocation(session, script.scriptID, candidate.Location)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[skip-breakpoint] %s: %v\n", candidate.Location.Mode, err)
				continue
			}
			setRaw, err := session.Send("Debugger.setBreakpoint", map[string]any{
				"location":  actualLoc,
				"condition": candidate.Condition,
			}, 5*time.Second)
			if err != nil {
				continue
			}
			var setRes struct {
				BreakpointID   string      `json:"breakpointId"`
				ActualLocation cdpLocation `json:"actualLocation"`
			}
			if err := json.Unmarshal(setRaw, &setRes); err == nil && setRes.BreakpointID != "" {
				armedBreakpoints = append(armedBreakpoints, armedBreakpoint{
					BundleURL:    script.url,
					Mode:         candidate.Location.Mode,
					Condition:    candidate.Condition,
					BreakpointID: setRes.BreakpointID,
					RequestedLocation: cdpLocation{
						LineNumber:   candidate.Location.LineNumber,
						ColumnNumber: candidate.Location.ColumnNumber,
					},
					ActualLocation: setRes.ActualLocation,
				})
			}
		}
	}

	if len(armedBreakpoints) == 0 {
		return errors.New("Debugger.setBreakpoint did not resolve an actual location")
	}

	primary := armedBreakpoints[0]
	readyDetails := map[string]any{
		"port":              port,
		"targetId":          target.ID,
		"targetUrl":         target.URL,
		"bundleUrl":         primary.BundleURL,
		"breakpointId":      primary.BreakpointID,
		"requestedLocation": primary.RequestedLocation,
		"actualLocation":    primary.ActualLocation,
		"breakpoints":       armedBreakpoints,
	}
	reportStatus(statusPath, "ready", readyDetails)
	if onReady != nil {
		onReady()
	}

	reloadDetected := int32(0)
	session.On("Runtime.executionContextsCleared", func(json.RawMessage) {
		atomic.StoreInt32(&reloadDetected, 1)
	})

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.closed:
			return errors.New("cdp session closed unexpectedly")
		case <-ticker.C:
			if atomic.LoadInt32(&reloadDetected) == 1 {
				return errors.New("execution context cleared (reload detected)")
			}
			val, err := session.Evaluate("(() => ({ alive: Boolean(globalThis.__codexExternalQuotaGate && (globalThis.__codexInputUnlockState?.breakpoint || globalThis.__codexExternalPatchState?.breakpoint)), stats: globalThis.__codexExternalQuotaGate?.stats ?? null }))()")
			if err != nil {
				return err
			}
			if m, ok := val.(map[string]any); ok {
				if alive, _ := m["alive"].(bool); !alive {
					return errors.New("unlock gate probe returned not alive")
				}
				readyDetails["gateStats"] = m["stats"]
				reportStatus(statusPath, "ready", readyDetails)
			}
		}
	}
}

// ============================================================================
// 4. Platform Application Launch & Detection
// ============================================================================

func getFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func findAppPath() (string, error) {
	if custom := os.Getenv("CODEX_APP_PATH"); custom != "" {
		if fi, err := os.Stat(custom); err == nil && fi.IsDir() {
			return custom, nil
		}
		return "", fmt.Errorf("CODEX_APP_PATH is invalid: %s", custom)
	}

	home, _ := os.UserHomeDir()
	var candidates []string
	if runtime.GOOS == "darwin" {
		candidates = []string{
			"/Applications/Codex.app",
			filepath.Join(home, "Applications/Codex.app"),
			"/Applications/ChatGPT.app",
			filepath.Join(home, "Applications/ChatGPT.app"),
		}
	} else if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		candidates = []string{
			filepath.Join(localAppData, "Programs", "OpenAI", "Codex", "Codex.exe"),
			filepath.Join(localAppData, "Programs", "OpenAI", "ChatGPT", "ChatGPT.exe"),
		}
	}

	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil {
			if runtime.GOOS == "darwin" && fi.IsDir() {
				return c, nil
			}
			if runtime.GOOS == "windows" && !fi.IsDir() {
				return c, nil
			}
		}
	}
	return "", errors.New("Codex.app or ChatGPT.app was not found. Set CODEX_APP_PATH.")
}

func getAppExecutable(appPath string) (string, error) {
	if runtime.GOOS == "darwin" {
		infoPlist := filepath.Join(appPath, "Contents", "Info.plist")
		data, err := os.ReadFile(infoPlist)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", infoPlist, err)
		}
		re := regexp.MustCompile(`<key>CFBundleExecutable</key>\s*<string>([^<]+)</string>`)
		m := re.FindStringSubmatch(string(data))
		if len(m) != 2 {
			return "", fmt.Errorf("cannot find CFBundleExecutable in %s", infoPlist)
		}
		bin := filepath.Join(appPath, "Contents", "MacOS", m[1])
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("executable not found: %s", bin)
		}
		return bin, nil
	}
	return appPath, nil
}

func isAppRunning(appPath string) bool {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("ps", "-axo", "command=").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), appPath+"/Contents/MacOS/")
	} else if runtime.GOOS == "windows" {
		base := filepath.Base(appPath)
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("IMAGENAME eq %s", base)).Output()
		if err != nil {
			return false
		}
		return strings.Contains(strings.ToLower(string(out)), strings.ToLower(base))
	}
	return false
}

// ============================================================================
// 5. 核心动作逻辑：launch, supervise, self-test, diagnose, verify, stop
// ============================================================================

// runSupervise 负责 CDP 调试端口的生命周期维护与断点保活
// 【关键特性】：
// 1. Electron 桌面应用内，用户随时可能按 Cmd+R 刷新页面或切换工作区导致前端页面重建；
// 2. 本守护循环会在连接断开或页面刷新后自动重新捕捉渲染目标并再次下发解锁断点；
// 3. 【自动退出感知】：一旦检测到客户端已被用户完全关闭（isAppRunning 返回 false），
//    守护循环会立即停止并退出，不留下任何后台驻留僵尸进程。
func runSupervise(ctx context.Context, port int, gateFile, statusPath string, startupTimeout time.Duration, appPath string) error {
	if appPath == "" {
		appPath, _ = findAppPath()
	}
	gateSource := embeddedRuntimeGate
	if gateFile != "" {
		if data, err := os.ReadFile(gateFile); err == nil {
			gateSource = string(data)
		} else {
			return fmt.Errorf("failed to read gate file %s: %w", gateFile, err)
		}
	}

	deadline := time.Now().Add(startupTimeout)
	everReady := false
	var missingSince time.Time
	var lastRetryError error

	reportStatus(statusPath, "waiting-for-codex", map[string]any{"port": port})

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		target, err := discoverTarget(port)
		if err != nil {
			// 如果主上下文已被取消，或者应用进程已关闭，立即优雅退出
			if ctx.Err() != nil {
				return nil
			}
			if everReady && appPath != "" && !isAppRunning(appPath) {
				return nil
			}
			if missingSince.IsZero() {
				missingSince = time.Now()
			}
			if !everReady && time.Now().After(deadline) {
				reportStatus(statusPath, "failed", map[string]any{"error": "Codex CDP startup timed out: " + err.Error()})
				return fmt.Errorf("Codex CDP startup timed out: %w", err)
			}
			// 之前已成功解锁过，但现在端口连续多秒失联且应用已退出，则视为用户主动退出
			if everReady && time.Since(missingSince) > 3*time.Second {
				if appPath != "" && !isAppRunning(appPath) {
					return nil
				}
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		missingSince = time.Time{}
		reportStatus(statusPath, "injecting", map[string]any{
			"port":      port,
			"targetId":  target.ID,
			"targetUrl": target.URL,
		})

		err = superviseTarget(ctx, target, gateSource, port, statusPath, func() {
			everReady = true
			fmt.Printf("Codex input unlock ready on port %d.\n", port)
		})

		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if everReady && appPath != "" && !isAppRunning(appPath) {
				return nil
			}
			if !everReady && time.Now().After(deadline) {
				reportStatus(statusPath, "failed", map[string]any{"error": err.Error()})
				return err
			}
			lastRetryError = err
			reportStatus(statusPath, "retrying", map[string]any{
				"port":  port,
				"error": lastRetryError.Error(),
			})
			fmt.Fprintf(os.Stderr, "[retry] %v\n", lastRetryError)
			time.Sleep(1 * time.Second)
		}
	}
}

// runSelfTest 对给定的前端 JS Bundle 文件进行离线断点规则匹配测试。
// 适用于 CI 或日常排查：在不拉起桌面客户端的情况下，离线验证当前规则引擎能否匹配最新的 Bundle。
func runSelfTest(bundlePath, gatePath string) error {
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return fmt.Errorf("failed to read bundle: %w", err)
	}
	source := string(data)
	sourceURL := "app://-/assets/" + filepath.Base(bundlePath)
	loc := locate(source, sourceURL)
	if loc == nil {
		rep := explain(source, sourceURL)
		repBytes, _ := json.Marshal(rep)
		return fmt.Errorf("quota gate did not uniquely locate the bundle: %s", string(repBytes))
	}
	cond := conditionForLocation(loc)
	rep := explain(source, sourceURL)

	out := map[string]any{
		"ok":        true,
		"location":  loc,
		"condition": cond,
		"explain":   rep,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// runLaunch 拉起桌面客户端并开启 CDP 守护进程（前台终端运行模式）
func runLaunch(gatePath, statusPath string, debugPort int, startupTimeoutSec int, killExisting bool) error {
	// 启动前检查新版本：一旦发现新版则更新且暂不拉起应用，更新完提示用户重新启动
	if checkPreLaunchUpdate() {
		return nil
	}

	appPath, err := findAppPath()
	if err != nil {
		return err
	}
	appBin, err := getAppExecutable(appPath)
	if err != nil {
		return err
	}

	if isAppRunning(appPath) {
		if killExisting {
			fmt.Println("Closing running instance...")
			if runtime.GOOS == "darwin" {
				_ = exec.Command("osascript", "-e", `tell application id "com.openai.codex" to quit`).Run()
				_ = exec.Command("osascript", "-e", `tell application id "com.openai.chat" to quit`).Run()
				time.Sleep(1 * time.Second)
			}
		} else {
			return fmt.Errorf("Codex/ChatGPT is already running. Please quit it completely, or re-run with --kill-existing")
		}
	}

	port := debugPort
	if port <= 0 {
		freePort, err := getFreePort()
		if err != nil {
			return fmt.Errorf("failed to allocate free port: %w", err)
		}
		port = freePort
	}

	if statusPath == "" {
		home, _ := os.UserHomeDir()
		statusDir := filepath.Join(home, "Library", "Application Support", "OpenAI", "CodexInputUnlock", "state")
		if runtime.GOOS == "windows" {
			statusDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "CodexInputUnlock", "state")
		}
		statusPath = filepath.Join(statusDir, fmt.Sprintf("runtime-%d.json", time.Now().UnixNano()))
	}

	// 注入远程调试参数启动客户端主进程
	args := []string{
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--remote-allow-origins=http://127.0.0.1:%d", port),
	}
	cmd := exec.Command(appBin, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch application: %w", err)
	}

	fmt.Printf("Launched %s (PID %d) on debug port %d\n", filepath.Base(appPath), cmd.Process.Pid, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 捕获 Ctrl+C 或 SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// 【全自动联动 1】：监控直接启动的客户端子进程结束
	go func() {
		_ = cmd.Wait()
		cancel()
	}()

	// 【全自动联动 2】：定期轮询应用是否运行（防客户端多进程替换）
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !isAppRunning(appPath) {
					cancel()
					return
				}
			}
		}
	}()

	return runSupervise(ctx, port, gatePath, statusPath, time.Duration(startupTimeoutSec)*time.Second, appPath)
}

// runLaunchDetached 以后台托管模式启动客户端（适用于桌面一键启动脚本）
// 【工作流程】：
// 1. 启动前快速检测 GitHub 新版本：若发现新版则立即更新，暂不启动客户端，并弹窗提示用户重新启动；
// 2. 若无新版，父进程在后台以独立的 Process Session 启动当前的二进制（Setsid / CREATE_NO_WINDOW）；
// 3. 父进程等待子进程通过 status 文件汇报 state=="ready"（约 0.5 ~ 1 秒）；
// 4. 一旦确认断点注入成功，在控制台打印成功提示并向桌面发送系统通知，然后父进程退出（exit 0）；
// 5. 终端窗口随之立即关闭，后台子进程继续保持静默守护，直到 ChatGPT 客户端被按 Cmd+Q 退出时自动关闭。
func runLaunchDetached(gatePath, statusPath string, debugPort int, startupTimeoutSec int, killExisting bool) error {
	// 启动前检查新版本：一旦发现新版则更新且暂不拉起应用，更新完提示用户重新启动
	if checkPreLaunchUpdate() {
		return nil
	}

	appPath, err := findAppPath()
	if err != nil {
		return err
	}

	if isAppRunning(appPath) {
		if killExisting {
			fmt.Println("Closing running instance...")
			if runtime.GOOS == "darwin" {
				_ = exec.Command("osascript", "-e", `tell application id "com.openai.codex" to quit`).Run()
				_ = exec.Command("osascript", "-e", `tell application id "com.openai.chat" to quit`).Run()
				time.Sleep(1 * time.Second)
			}
		} else {
			return fmt.Errorf("Codex/ChatGPT is already running. Please quit it completely, or re-run with --kill-existing")
		}
	}

	if statusPath == "" {
		home, _ := os.UserHomeDir()
		statusDir := filepath.Join(home, "Library", "Application Support", "OpenAI", "CodexInputUnlock", "state")
		if runtime.GOOS == "windows" {
			statusDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "CodexInputUnlock", "state")
		}
		_ = os.MkdirAll(statusDir, 0755)
		statusPath = filepath.Join(statusDir, fmt.Sprintf("runtime-%d.json", time.Now().UnixNano()))
	}

	selfBin, err := os.Executable()
	if err != nil {
		selfBin = os.Args[0]
	}

	subArgs := []string{"--status=" + statusPath}
	if debugPort > 0 {
		subArgs = append(subArgs, fmt.Sprintf("--port=%d", debugPort))
	}
	if gatePath != "" {
		subArgs = append(subArgs, "--gate="+gatePath)
	}
	subArgs = append(subArgs, "launch")

	bgCmd := exec.Command(selfBin, subArgs...)
	prepareDetachedCmd(bgCmd)

	if err := bgCmd.Start(); err != nil {
		return fmt.Errorf("failed to start supervisor in background: %w", err)
	}

	fmt.Println("正在启动 Codex / ChatGPT 并注入解锁补丁...")

	deadline := time.Now().Add(time.Duration(startupTimeoutSec) * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(statusPath)
		if err == nil {
			var st map[string]any
			if json.Unmarshal(data, &st) == nil {
				stVal, _ := st["state"].(string)
				if stVal == "" {
					stVal, _ = st["phase"].(string)
				}
				if stVal == "ready" {
					ready = true
					break
				}
				if stVal == "failed" {
					return fmt.Errorf("startup failed: %v", st["error"])
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	if !ready {
		return fmt.Errorf("startup timed out waiting for supervisor to be ready")
	}

	fmt.Println("✓ Codex / ChatGPT 已成功启动，输入框解锁补丁已生效！")
	fmt.Println("✓ 守护程序已在后台运行（在您按 Cmd+Q 退出 ChatGPT 软件时会自动跟随关闭）。")

	if runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e", `display notification "ChatGPT 输入框已解锁就绪，退出软件时自动清理" with title "Codex Input Unlock"`).Run()
	}

	return nil
}

func runStop() error {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e", `tell application id "com.openai.codex" to quit`).Run()
		_ = exec.Command("osascript", "-e", `tell application id "com.openai.chat" to quit`).Run()
	}
	currentPid := os.Getpid()
	stopAllUnlockProcesses(currentPid)
	fmt.Println("已停止 Codex / ChatGPT 及解锁守护进程。")
	return nil
}

func runDiagnose() {
	app, err := findAppPath()
	fmt.Printf("OS: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fmt.Printf("App: not found (%v)\n", err)
	} else {
		fmt.Printf("App: %s\n", app)
		running := isAppRunning(app)
		fmt.Printf("Running: %v\n", running)
	}
}

func runVerify(port int, statusPath string) error {
	if statusPath != "" {
		if data, err := os.ReadFile(statusPath); err == nil {
			fmt.Printf("Status file content:\n%s\n", string(data))
		}
	}
	if port > 0 {
		target, err := discoverTarget(port)
		if err != nil {
			return fmt.Errorf("CDP port %d not responding: %w", port, err)
		}
		fmt.Printf("CDP responding OK on port %d: target %s (%s)\n", port, target.ID, target.URL)
	}
	return nil
}

// ============================================================================
// 5.5. 智能在线自更新机制 (Auto Update & Version Management)
// ============================================================================

// isNewerVersion 比较语义化版本号，判断 remote 是否高于 local (支持 v1.0.1 或 1.0.1 格式)
func isNewerVersion(remote, local string) bool {
	clean := func(v string) []int {
		v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
		parts := strings.Split(v, "-")[0] // 剥离预发布标签如 -rc1
		segs := strings.Split(parts, ".")
		nums := make([]int, len(segs))
		for i, s := range segs {
			n, _ := strconv.Atoi(s)
			nums[i] = n
		}
		return nums
	}
	rNums := clean(remote)
	lNums := clean(local)
	maxLen := len(rNums)
	if len(lNums) > maxLen {
		maxLen = len(lNums)
	}
	for i := 0; i < maxLen; i++ {
		rVal := 0
		if i < len(rNums) {
			rVal = rNums[i]
		}
		lVal := 0
		if i < len(lNums) {
			lVal = lNums[i]
		}
		if rVal > lVal {
			return true
		} else if rVal < lVal {
			return false
		}
	}
	return false
}

// fetchLatestReleaseTag 请求 GitHub 最新 Release 重定向，无额度限制地快速获取最新 Tag 名称
func fetchLatestReleaseTag(repo string, timeout time.Duration) (string, error) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 拦截 302 重定向，直接从 Location 响应头中读取重定向的目标 Release Tag
			return http.ErrUseLastResponse
		},
	}
	url := fmt.Sprintf("https://github.com/%s/releases/latest", repo)
	resp, err := client.Head(url)
	if err != nil {
		resp, err = client.Get(url)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("no redirect location found in latest release")
	}
	parts := strings.Split(strings.TrimRight(loc, "/"), "/")
	tag := parts[len(parts)-1]
	if tag == "" || tag == "latest" {
		return "", fmt.Errorf("unexpected release redirect destination: %s", loc)
	}
	return tag, nil
}

// downloadAndApplyUpdate 下载目标版本的可执行二进制，并在本地完成原子替换
func downloadAndApplyUpdate(repo, tag string, foreground bool) error {
	var assetName string
	switch runtime.GOOS {
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			assetName = "codex-input-unlock-darwin-arm64"
		case "amd64":
			assetName = "codex-input-unlock-darwin-amd64"
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			assetName = "codex-input-unlock-windows-amd64.exe"
		case "arm64":
			assetName = "codex-input-unlock-windows-arm64.exe"
		}
	}
	if assetName == "" {
		return fmt.Errorf("unsupported platform for auto update: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	downloadURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, assetName)
	if foreground {
		fmt.Printf("正在从 %s 下载新版本 %s...\n", repo, tag)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	if realPath, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = realPath
	}

	tempFile := execPath + fmt.Sprintf(".new-%d", time.Now().UnixNano())
	f, err := os.OpenFile(tempFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(tempFile)
		return err
	}

	// 替换运行中程序：
	// Windows 对运行中文件加锁不允许直接覆写，但允许重命名为 .old 后再写入新文件；
	// POSIX/Unix 系统支持原子直接 Rename 替换当前 Inode。
	if runtime.GOOS == "windows" {
		oldFile := execPath + ".old"
		_ = os.Remove(oldFile)
		if err := os.Rename(execPath, oldFile); err != nil {
			_ = os.Remove(tempFile)
			return err
		}
	}
	if err := os.Rename(tempFile, execPath); err != nil {
		return err
	}

	if foreground {
		fmt.Printf("✓ 升级完成！已成功更新至 %s\n", tag)
	}
	return nil
}

// checkPreLaunchUpdate 在拉起应用前快速检测是否有新版本。
// 【产品逻辑】：一旦在后台发现新版，就立即下载更新且暂不启动应用，更新完成后提示用户重新启动。
// 若无新版或检测超时（1.5 秒极速探测），返回 false 顺畅进入应用启动流程。
func checkPreLaunchUpdate() bool {
	repo := os.Getenv("CODEX_UNLOCK_RELEASE_REPO")
	if repo == "" {
		repo = "kaliluying/codex-input-unlock"
	}
	tag, err := fetchLatestReleaseTag(repo, 1500*time.Millisecond)
	if err != nil || !isNewerVersion(tag, appVersion) {
		return false
	}

	fmt.Printf("发现新版本 %s (当前版本: v%s)，正在为您更新...\n", tag, appVersion)
	if err := downloadAndApplyUpdate(repo, tag, true); err != nil {
		fmt.Fprintf(os.Stderr, "自动更新失败: %v。将继续使用当前版本启动...\n", err)
		return false
	}

	fmt.Println("\n==================================================")
	fmt.Printf("✓ 升级完成！已成功更新至最新版本 %s\n", tag)
	fmt.Println("请重新启动即可享受最新版本。")
	fmt.Println("==================================================")

	if runtime.GOOS == "darwin" {
		alertScript := fmt.Sprintf(`display alert "Codex 解锁补丁已更新至 %s" message "更新已就绪，请重新点击启动！" as informational`, tag)
		_ = exec.Command("osascript", "-e", alertScript).Run()
	}
	return true
}

// runUpdate 手动检查并执行在线升级
func runUpdate() error {
	repo := os.Getenv("CODEX_UNLOCK_RELEASE_REPO")
	if repo == "" {
		repo = "kaliluying/codex-input-unlock"
	}
	fmt.Printf("当前版本: v%s\n", appVersion)
	fmt.Printf("正在检查 %s 的最新发布版本...\n", repo)
	tag, err := fetchLatestReleaseTag(repo, 5*time.Second)
	if err != nil {
		return fmt.Errorf("检查更新失败: %w", err)
	}
	if !isNewerVersion(tag, appVersion) {
		fmt.Printf("当前已是最新版本 (%s)，无需更新。\n", tag)
		return nil
	}
	fmt.Printf("发现新版本: %s (当前: v%s)\n", tag, appVersion)
	return downloadAndApplyUpdate(repo, tag, true)
}

// ============================================================================
// 6. 命令行解析与程序入口 (CLI Entry Point)
// ============================================================================

func main() {
	var (
		portFlag           = flag.Int("port", 0, "CDP 远程调试端口 (默认自动分配空闲端口)")
		gateFlag           = flag.String("gate", "", "自定义 quota-gate.cjs 文件路径 (可选，默认已内置最新运行时)")
		statusFlag         = flag.String("status", "", "状态输出 JSON 文件持久化路径")
		startupTimeoutFlag = flag.Int("startup-timeout", 60, "启动与连接超时时长 (秒)")
		bundleFlag         = flag.String("bundle", "", "用于 --self-test 的前端 JS Bundle 文件路径")
		selfTestFlag       = flag.Bool("self-test", false, "离线测试当前规则引擎对指定 Bundle 的断点定位与条件提取")
		killExistingFlag   = flag.Bool("kill-existing", false, "若检测到客户端已有实例在运行，先自动退出旧实例")
		detachFlag         = flag.Bool("detach", false, "以后台静默方式托管运行，并在断点就绪后立刻退出终端控制台")
	)
	flag.Parse()

	args := flag.Args()
	cmd := ""
	isDetached := *detachFlag
	// 兼容任意位置出现的 --detach 或子命令
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if arg == "--detach" || arg == "-detach" {
				isDetached = true
			}
			if arg == "-v" || arg == "--version" || arg == "-version" {
				cmd = "version"
			}
			continue
		}
		if cmd == "" {
			cmd = arg
		}
	}

	// 子命令 0: version (打印当前程序版本)
	if cmd == "version" {
		fmt.Printf("codex-input-unlock v%s (%s/%s)\n", appVersion, runtime.GOOS, runtime.GOARCH)
		return
	}

	// 子命令 0.5: update (手动检查并升级到最新版本)
	if cmd == "update" {
		if err := runUpdate(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 子命令 1: stop (一键清理运行中的客户端与守护进程)
	if cmd == "stop" {
		if err := runStop(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 子命令 2: self-test (离线规则测试)
	if *selfTestFlag || cmd == "self-test" {
		bPath := *bundleFlag
		if bPath == "" && len(args) > 1 {
			bPath = args[1]
		}
		if bPath == "" {
			fmt.Fprintln(os.Stderr, "Error: --bundle <path> is required for self-test")
			os.Exit(1)
		}
		if err := runSelfTest(bPath, *gateFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 子命令 3: diagnose (检查环境与客户端安装路径)
	if cmd == "diagnose" {
		runDiagnose()
		return
	}

	// 子命令 4: verify (检查指定端口或状态文件的 CDP 连接与断点就绪状态)
	if cmd == "verify" {
		if err := runVerify(*portFlag, *statusFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 子命令 5: supervise (由外部启动器调用，接管已存在的 CDP 调试端口)
	if cmd == "supervise" || (*portFlag > 0 && *statusFlag != "") {
		if *portFlag <= 0 {
			fmt.Fprintln(os.Stderr, "Error: --port is required for supervise")
			os.Exit(1)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()
		if err := runSupervise(ctx, *portFlag, *gateFlag, *statusFlag, time.Duration(*startupTimeoutFlag)*time.Second, ""); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 默认动作: launch (拉起客户端并自动注入守护)
	if isDetached {
		if err := runLaunchDetached(*gateFlag, *statusFlag, *portFlag, *startupTimeoutFlag, *killExistingFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runLaunch(*gateFlag, *statusFlag, *portFlag, *startupTimeoutFlag, *killExistingFlag); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
