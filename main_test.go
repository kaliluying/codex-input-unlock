package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSmokeMockBundles 针对 6 种典型的 ChatGPT/Codex 前端 Webpack 打包变体进行离线回归测试。
// 验证每一种历史/现状模式的定位器（Locator）能否准确定位断点，并生成正确的解锁副作用表达式。
func TestSmokeMockBundles(t *testing.T) {
	// 用例 1: 当前版本 Codex 主聊天窗口 Bundle (codex-local-quota)
	currentCodexBundle := strings.Join([]string{
		"XP=Xl(K,({get:e})=>{let t=e(Ey),n=e(ly).data;if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
		"vn=X(bt===`local`&&XP),",
		"let Zn=De||gn||vn,Qn;",
		"submitDisabled:Zn",
	}, "")
	loc := locate(currentCodexBundle, "app://-/assets/app-primary-current.js")
	if loc == nil || loc.Mode != "codex-local-quota" || loc.QuotaVariable != "vn" || loc.HostVariable != "bt" {
		t.Fatalf("Failed currentCodexBundle: %+v", loc)
	}
	if cond := conditionForLocation(loc); cond != "(vn=false,false)" {
		t.Fatalf("Failed condition for currentCodexBundle: got %s", cond)
	}

	// 用例 2: 早期版本 Codex Bundle (表达式顺序略有差异)
	previousCodexBundle := strings.Replace(
		currentCodexBundle,
		"vn=X(bt===`local`&&XP),",
		"vn=X(XP)&&bt===`local`,",
		1,
	)
	prevLoc := locate(previousCodexBundle, "app://-/assets/app-primary-previous.js")
	if prevLoc == nil || prevLoc.Mode != "codex-local-quota" {
		t.Fatalf("Failed previousCodexBundle: %+v", prevLoc)
	}

	// 用例 3: 状态派生对象 Bundle (codex-rate-limit-state)
	rateLimitStateBundle := strings.Join([]string{
		"function VF(e,t){let o=WF(e,t),l=o?.blocked_features?.filter(e=>e.name===`send`),d=l?.find(...)??null;return{",
		"sendBlockReason:d?.block_reason??null,sendBlocked:d!=null}}",
	}, "")
	stateLoc := locate(rateLimitStateBundle, "app://-/assets/app-initial-next.js")
	if stateLoc == nil || stateLoc.Mode != "codex-rate-limit-state" || stateLoc.QuotaVariable != "d" {
		t.Fatalf("Failed rateLimitStateBundle: %+v", stateLoc)
	}
	if cond := conditionForLocation(stateLoc); cond != "(d=null,false)" {
		t.Fatalf("Failed condition for rateLimitStateBundle: got %s", cond)
	}

	// 用例 4: 派生原子 Bundle (codex-rate-limit-derived)
	derivedBundle := strings.Join([]string{
		"lV=z(Z,({get:e})=>{let t=e(Xb),n=e(Nb).data;",
		"if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
	}, "")
	derLoc := locate(derivedBundle, "app://-/assets/app-primary-next.js")
	if derLoc == nil || derLoc.Mode != "codex-rate-limit-derived" || derLoc.QuotaVariable != "t" {
		t.Fatalf("Failed derivedBundle: %+v", derLoc)
	}
	if cond := conditionForLocation(derLoc); cond != "(t={},false)" {
		t.Fatalf("Failed condition for derivedBundle: got %s", cond)
	}

	// 用例 5: 最新双重混淆 Bundle (codex-rate-limit-derived-v2)
	derivedV2Bundle := strings.Join([]string{
		"a6kU(e,t,n){var a=n(`y5DF`),i=n(`SPnB`);",
		"let c=(0,a.un)(i.u,({get:e})=>{let t=e(i.PO),n=e(d.Ix).data;",
		`if("chatgpt"!==t.authMethod||t.authLoading||n.rate_limit?.allowed!==!1)return!1;return!0})}`,
	}, "")
	derV2Loc := locate(derivedV2Bundle, "app://-/webview/assets/async/465814.879bc2db3c.js")
	if derV2Loc == nil || derV2Loc.Mode != "codex-rate-limit-derived-v2" || derV2Loc.QuotaVariable != "t" {
		t.Fatalf("Failed derivedV2Bundle: %+v", derV2Loc)
	}
	if cond := conditionForLocation(derV2Loc); cond != "(t={},false)" {
		t.Fatalf("Failed condition for derivedV2Bundle: got %s", cond)
	}

	// 用例 6: 原子数据守护模式 (quota-atom-data)
	atomGuardBundle := strings.Join([]string{
		"XP=Xl(K,({get:e})=>{let t=e(Ey),n=e(ly).data;if(t.authMethod!==`chatgpt`||n.rate_limit?.allowed!==!1)return!1;return!0})",
	}, "")
	all := locateAll(atomGuardBundle, "app://-/assets/app-primary-atom.js")
	var atomGuard *Location
	var atomCond string
	for _, entry := range all {
		if entry.Location.Mode == "quota-atom-data" {
			atomGuard = &entry.Location
			atomCond = entry.Condition
			break
		}
	}
	if atomGuard == nil || atomGuard.QuotaVariable != "n" || atomCond != "(__codexExternalQuotaGate.unlock(n),false)" {
		t.Fatalf("Failed atomGuard: %+v, cond: %s", atomGuard, atomCond)
	}
}

// TestConditionValidation 测试变量名安全校验函数，防止恶意/异常的混淆变量名导致 JS 语法注入
func TestConditionValidation(t *testing.T) {
	badLoc := &Location{Mode: "codex-local-quota", QuotaVariable: "bad-name;"}
	if cond := conditionForLocation(badLoc); cond != "" {
		t.Fatalf("Expected empty condition for unsafe identifier, got: %s", cond)
	}
}

// Electron asar 归档包文件结构映射
type asarEntry struct {
	Size   int64                 `json:"size"`
	Offset string                `json:"offset"`
	Files  map[string]*asarEntry `json:"files"`
}

type asarHeader struct {
	Files map[string]*asarEntry `json:"files"`
}

// TestRealAppAsarIfAvailable 是真正的端到端真实测试：
// 如果本机安装了 ChatGPT.app / Codex.app，本测试会直接读取其 Resources/app.asar，
// 用纯 Go 解析 asar 二进制包头（读取 JSON 文件目录索引），提取包含 rate_limit 的真实代码段，
// 验证 Go 实现的正则定位器能否 100% 成功识别真实应用中的混淆 JS 代码。
func TestRealAppAsarIfAvailable(t *testing.T) {
	app, err := findAppPath()
	if err != nil {
		t.Skip("No local Codex/ChatGPT app installed")
	}
	asarPath := filepath.Join(app, "Contents", "Resources", "app.asar")
	buf, err := os.ReadFile(asarPath)
	if err != nil {
		t.Skipf("app.asar not readable: %v", err)
	}
	if len(buf) < 16 {
		t.Fatalf("app.asar too small")
	}
	// asar 文件前 16 字节为头标记与头长度（Little Endian）
	headerSize := binary.LittleEndian.Uint32(buf[12:16])
	dataStart := int64(16 + headerSize)
	headerBytes := buf[16 : 16+headerSize]

	var header asarHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshal header failed: %v", err)
	}

	matchedCount := 0
	var walk func(entries map[string]*asarEntry, prefix string)
	walk = func(entries map[string]*asarEntry, prefix string) {
		for name, entry := range entries {
			fp := prefix + "/" + name
			if entry.Files != nil {
				walk(entry.Files, fp)
				continue
			}
			if !strings.HasSuffix(name, ".js") {
				continue
			}
			offset, _ := strconv.ParseInt(entry.Offset, 10, 64)
			start := dataStart + offset
			end := start + entry.Size
			if end > int64(len(buf)) {
				continue
			}
			fileData := buf[start:end]
			// 仅针对包含额度限制逻辑的核心 bundle 进行探测
			if !strings.Contains(string(fileData), "rate_limit?.allowed!==!1") {
				continue
			}
			text := string(fileData)
			url := "app://-" + fp
			rep := explain(text, url)
			if len(rep.Matched) > 0 {
				matchedCount++
				for _, m := range rep.Matched {
					if m != "codex-rate-limit-derived-v2" {
						t.Errorf("Unexpected mode %s for bundle %s", m, fp)
					}
				}
			}
		}
	}
	walk(header.Files, "")

	if matchedCount == 0 {
		t.Fatalf("Expected at least 1 matched bundle in %s, got 0", asarPath)
	}
	t.Logf("Successfully matched %d bundles in %s", matchedCount, asarPath)
}

