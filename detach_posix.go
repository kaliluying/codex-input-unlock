//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// prepareDetachedCmd 在 POSIX/Unix 系统 (macOS/Linux) 下配置子进程的系统属性。
// Setsid: true 将子进程置于一个全新的 Session 和进程组中，彻底与当前控制台终端解绑，
// 这样当父进程退出、终端窗口关闭或收到 SIGHUP 时，后台守护进程依然能独立存活。
func prepareDetachedCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

// stopAllUnlockProcesses 在 POSIX 系统下查找并优雅终止其他正在运行的解锁守护进程
func stopAllUnlockProcesses(currentPid int) {
	out, err := exec.Command("pgrep", "-f", "codex-input-unlock").Output()
	if err == nil {
		pids := strings.Fields(string(out))
		for _, pidStr := range pids {
			var pid int
			if _, err := fmt.Sscanf(pidStr, "%d", &pid); err == nil && pid != currentPid {
				proc, err := os.FindProcess(pid)
				if err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
			}
		}
	}
}

// restartSelf 在 POSIX/Unix 系统下，通过 syscall.Exec 使用新下载的二进制原地替换当前进程映像，
// 完美继承当前的控制台会话、PID 与命令行参数，实现无感的全自动重启并拉起应用。
func restartSelf(execPath string, args []string, env []string) error {
	return syscall.Exec(execPath, args, env)
}



