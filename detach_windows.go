//go:build windows

package main

import (
	"os"
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

// restartSelf 在 Windows 系统下，通过拉起新下载的二进制进程并退出当前进程的方式完成自身自动重启。
func restartSelf(execPath string, args []string, env []string) error {
	var cmdArgs []string
	if len(args) > 1 {
		cmdArgs = args[1:]
	}
	cmd := exec.Command(execPath, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}



