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

