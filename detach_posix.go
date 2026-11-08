//go:build !windows

package main

import (
	"os/exec"
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

