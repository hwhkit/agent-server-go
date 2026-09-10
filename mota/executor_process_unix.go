//go:build unix

package mota

// 进程组管理(Unix)。对应 Rust 侧 process.rs 的 cfg(unix) 分支。
//
// 子进程被放进自己的进程组,这样超时清理可以用负 PID 一次性覆盖它 fork 出的
// 全部后代 —— 本机 agent CLI 常会拉起 node/python 子进程。

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup 让子进程成为新进程组的组长。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// processGroupGuard 持有子进程组 ID,负责在退出路径上清理整组。
type processGroupGuard struct{ pgid int }

// newProcessGroupGuard 从已启动的子进程取进程组 ID。
func newProcessGroupGuard(cmd *exec.Cmd) *processGroupGuard {
	if cmd.Process == nil {
		return &processGroupGuard{}
	}
	// Setpgid 时子进程 PID 即其进程组 ID。
	return &processGroupGuard{pgid: cmd.Process.Pid}
}

// Terminate 给整个进程组发 SIGTERM,让子进程有机会自己收尾。
func (g *processGroupGuard) Terminate() {
	if g.pgid > 0 {
		_ = syscall.Kill(-g.pgid, syscall.SIGTERM)
	}
}

// KillRemaining 给整个进程组发 SIGKILL,回收仍持有继承管道的后代。
func (g *processGroupGuard) KillRemaining() {
	if g.pgid > 0 {
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
	}
}

// Disarm 放弃该进程组,后续清理调用变为空操作。
func (g *processGroupGuard) Disarm() { g.pgid = 0 }
