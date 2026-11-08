//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// prepareDetachedCmd 在 Windows 系统下配置子进程的创建标志。
// CreationFlags: 0x08000000 即 CREATE_NO_WINDOW，
// 确保子进程启动时不弹出黑色的 cmd 控制台窗口，实现完全静默的后台守护运行。
func prepareDetachedCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// stopAllUnlockProcesses 在 Windows 系统下调用 taskkill 结束残留的解锁守护进程
func stopAllUnlockProcesses(currentPid int) {
	_ = exec.Command("taskkill", "/F", "/IM", "codex-input-unlock.exe").Run()
}


