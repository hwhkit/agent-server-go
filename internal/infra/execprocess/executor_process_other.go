//go:build !unix

package execprocess

// 进程组管理(非 Unix)。对应 Rust 侧 process.rs 的 cfg(not(unix)) 分支:
// 没有进程组语义,清理退化为只处理直接子进程(由 RunCommand 的 Kill 兜底)。

import "os/exec"

// configureProcessGroup 在非 Unix 平台上是空操作。
func configureProcessGroup(_ *exec.Cmd) {}

// processGroupGuard 在非 Unix 平台上不持有任何状态。
type processGroupGuard struct{}

func newProcessGroupGuard(_ *exec.Cmd) *processGroupGuard { return &processGroupGuard{} }

func (g *processGroupGuard) Terminate()     {}
func (g *processGroupGuard) KillRemaining() {}
func (g *processGroupGuard) Disarm()        {}
