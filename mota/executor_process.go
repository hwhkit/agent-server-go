package mota

// 本机 Agent 子进程生命周期:限时、限流读取、进程组清理。
// 对应 Rust 侧 src/platform/executor/process.rs。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"
)

// OutputCapture 决定如何抓取子进程 stdout。
type OutputCapture int

const (
	// CaptureComplete 要求完整输出落在上限内;适用于单个 JSON/文本结果。
	CaptureComplete OutputCapture = iota
	// CaptureEventStreamTail 持续排空事件流,只保留最后 outputLimit 字节;
	// 适用于 Pi/Codex 的 JSONL 流。
	CaptureEventStreamTail
)

// processTerminationGrace 是 SIGTERM 之后等待子进程自行退出的时间。
const processTerminationGrace = 2 * time.Second

// runCommand 启动子进程并在 timeout 内收集其 stdout。
//
// 无论是否超时,stdout 与 stderr 都会被持续排空,避免 verbose CLI 因管道写满而
// 阻塞。stderr 只用于诊断,不参与岗位结果解析。
func runCommand(
	cmd *exec.Cmd,
	stdinPayload []byte,
	timeout time.Duration,
	outputLimit int,
	capture OutputCapture,
	label string,
) ([]byte, error) {
	var stdoutSink boundedSink
	if capture == CaptureEventStreamTail {
		stdoutSink = newTailSink(outputLimit)
	} else {
		stdoutSink = newPrefixSink(outputLimit)
	}
	stderrSink := newPrefixSink(outputLimit)

	// 三条 stdio 都用显式 os.Pipe,并把写端以 *os.File 交给子进程。这样 os/exec
	// 不会注册内部拷贝协程,cmd.Wait 只等进程本身退出 —— 与 Rust 的
	// child.wait() 语义一致。
	//
	// 这一点是必需的:若把 io.Writer 交给 cmd.Stdout,os/exec 会等拷贝协程结束,
	// 而一个仍持有继承管道的孙进程会让 Wait 永久阻塞,直接掉进假超时。
	pipes, err := newStdioPipes()
	if err != nil {
		return nil, newExecutionError("executor_unavailable",
			fmt.Sprintf("create %s stdio pipes: %v", label, err))
	}
	defer pipes.CloseAll()

	cmd.Stdin = pipes.stdinRead
	cmd.Stdout = pipes.stdoutWrite
	cmd.Stderr = pipes.stderrWrite

	configureProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, newExecutionError("executor_unavailable", fmt.Sprintf("start %s: %v", label, err))
	}
	guard := newProcessGroupGuard(cmd)
	// 兜底:任何提前返回的路径都不留下孤儿进程组。
	defer guard.KillRemaining()

	// 关掉父进程侧的子端 fd,否则读端永远等不到 EOF。
	pipes.CloseChildEnds()

	// 写完 payload 立即关闭 stdin,这正是 Codex 的 `-` 读取模式能正常结束的前提。
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		defer pipes.stdinWrite.Close()
		if len(stdinPayload) > 0 {
			// 子进程不读 stdin 时会得到 EPIPE,这不是错误。
			_, _ = pipes.stdinWrite.Write(stdinPayload)
		}
	}()

	// 始终排空 stdout/stderr,避免 verbose CLI 因管道写满而阻塞。
	stdoutDone := drainInto(stdoutSink, pipes.stdoutRead)
	stderrDone := drainInto(stderrSink, pipes.stderrRead)

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waitDone:
	case <-timer.C:
		terminateChildTree(cmd, guard, waitDone)
		timedOut = true
	}

	// 进程已退出;SIGKILL 整个进程组,回收仍持有继承管道的孙进程,读端随之 EOF。
	guard.KillRemaining()
	guard.Disarm()

	// 收拢读协程。若有进程逃出了进程组,用读超时强制解阻塞,绝不无限等待。
	pipes.AwaitDrain(stdoutDone, stderrDone, stdinDone)

	if timedOut {
		return nil, newExecutionError("execution_timeout",
			fmt.Sprintf("%s exceeded its deadline", label))
	}

	stdout, stdoutTruncated := stdoutSink.Bytes(), stdoutSink.Truncated()
	stderr, stderrTruncated := stderrSink.Bytes(), stderrSink.Truncated()

	if stderrTruncated {
		slog.Warn("local Agent stderr exceeded capture limit; excess bytes discarded",
			"executor", label, "output_limit", outputLimit)
	}
	if stdoutTruncated && capture == CaptureComplete {
		return nil, newExecutionError("output_too_large",
			fmt.Sprintf("%s stdout exceeded the configured limit", label)).
			WithExecutorOutput(capturedProcessOutput(stdout, stderr, stdoutTruncated, stderrTruncated))
	}
	if stdoutTruncated {
		slog.Info("local Agent event stream exceeded capture limit; parsed from bounded tail",
			"executor", label, "output_limit", outputLimit)
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			return nil, newExecutionError("executor_failed",
				fmt.Sprintf("%s exited with status %s", label, exitError.ProcessState.String())).
				WithExecutorOutput(capturedProcessOutput(stdout, stderr, stdoutTruncated, stderrTruncated))
		}
		return nil, newExecutionError("executor_failed",
			fmt.Sprintf("wait for %s: %v", label, waitErr))
	}
	return stdout, nil
}

// terminateChildTree 在超时后逐级升级信号:SIGTERM → 2s 宽限 → SIGKILL 进程组
// → 直接 kill 子进程。
func terminateChildTree(cmd *exec.Cmd, guard *processGroupGuard, waitDone <-chan error) {
	guard.Terminate()

	graceTimer := time.NewTimer(processTerminationGrace)
	defer graceTimer.Stop()
	exited := false
	select {
	case <-waitDone:
		exited = true
	case <-graceTimer.C:
	}

	guard.KillRemaining()
	if !exited {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		// 等 Wait 收尸;进程组已被 SIGKILL,这里不会久等。
		hardTimer := time.NewTimer(processTerminationGrace)
		defer hardTimer.Stop()
		select {
		case <-waitDone:
		case <-hardTimer.C:
		}
	}
	guard.Disarm()
}

// capturedProcessOutput 生成仅用于运行记录审计的进程输出快照。
func capturedProcessOutput(stdout, stderr []byte, stdoutTruncated, stderrTruncated bool) map[string]any {
	return map[string]any{
		"stdout":          lossyUTF8(stdout),
		"stderr":          lossyUTF8(stderr),
		"stdoutTruncated": stdoutTruncated,
		"stderrTruncated": stderrTruncated,
	}
}

// lossyUTF8 把任意字节按 UTF-8 解码,非法序列替换为 U+FFFD。
// 对应 Rust 侧 String::from_utf8_lossy。
func lossyUTF8(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	var builder bytes.Buffer
	builder.Grow(len(raw))
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		if r == utf8.RuneError && size == 1 {
			builder.WriteRune(utf8.RuneError)
		} else {
			builder.Write(raw[:size])
		}
		raw = raw[size:]
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// 有界输出捕获
// ---------------------------------------------------------------------------

// boundedSink 是有界输出接收器。Write 永远报告写入全部字节,让 os/exec 的拷贝
// 协程持续排空管道,即使超出上限也不阻塞子进程。
type boundedSink interface {
	io.Writer
	// Bytes 返回保留下来的字节。
	Bytes() []byte
	// Truncated 表示总写入量是否超过上限。
	Truncated() bool
}

// prefixSink 只保留前 limit 字节。
type prefixSink struct {
	limit int
	buf   []byte
	total int
}

func newPrefixSink(limit int) *prefixSink {
	if limit < 0 {
		limit = 0
	}
	capacity := limit
	if capacity > 64*1024 {
		capacity = 64 * 1024
	}
	return &prefixSink{limit: limit, buf: make([]byte, 0, capacity)}
}

func (s *prefixSink) Write(p []byte) (int, error) {
	s.total += len(p)
	if remaining := s.limit - len(s.buf); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		s.buf = append(s.buf, p[:remaining]...)
	}
	return len(p), nil
}

func (s *prefixSink) Bytes() []byte   { return s.buf }
func (s *prefixSink) Truncated() bool { return s.total > s.limit }

// tailSink 用环形缓冲只保留最后 limit 字节,保证 JSONL 解析器仍能看到收尾事件
// (Pi 的 message_end / Codex 的 turn.completed)。
type tailSink struct {
	limit   int
	ring    []byte
	filled  int
	writeAt int
	total   int
}

func newTailSink(limit int) *tailSink {
	if limit < 0 {
		limit = 0
	}
	return &tailSink{limit: limit, ring: make([]byte, limit)}
}

func (s *tailSink) Write(p []byte) (int, error) {
	s.total += len(p)
	if s.limit == 0 {
		return len(p), nil
	}
	// 单次写入就超过整个窗口:直接用它的尾部覆盖全环。
	if len(p) >= s.limit {
		copy(s.ring, p[len(p)-s.limit:])
		s.filled = s.limit
		s.writeAt = 0
		return len(p), nil
	}
	first := s.limit - s.writeAt
	if len(p) < first {
		first = len(p)
	}
	copy(s.ring[s.writeAt:s.writeAt+first], p[:first])
	if second := len(p) - first; second > 0 {
		copy(s.ring[:second], p[first:])
	}
	s.writeAt = (s.writeAt + len(p)) % s.limit
	s.filled += len(p)
	if s.filled > s.limit {
		s.filled = s.limit
	}
	return len(p), nil
}

func (s *tailSink) Bytes() []byte {
	if s.limit == 0 {
		return nil
	}
	if s.filled < s.limit {
		return s.ring[:s.filled]
	}
	// 环已满:从写指针处旋转,让字节回到时间顺序。
	tail := make([]byte, 0, s.limit)
	tail = append(tail, s.ring[s.writeAt:]...)
	tail = append(tail, s.ring[:s.writeAt]...)
	return tail
}

func (s *tailSink) Truncated() bool { return s.total > s.limit }

// ---------------------------------------------------------------------------
// stdio 管道
// ---------------------------------------------------------------------------

// drainDeadline 是读协程在进程组清理后仍未 EOF 时的强制解阻塞时限。
const drainDeadline = 2 * time.Second

// stdioPipes 持有三条显式管道的两端。
type stdioPipes struct {
	stdinRead, stdinWrite   *os.File
	stdoutRead, stdoutWrite *os.File
	stderrRead, stderrWrite *os.File
}

func newStdioPipes() (*stdioPipes, error) {
	pipes := &stdioPipes{}
	var err error
	if pipes.stdinRead, pipes.stdinWrite, err = os.Pipe(); err != nil {
		return nil, err
	}
	if pipes.stdoutRead, pipes.stdoutWrite, err = os.Pipe(); err != nil {
		pipes.CloseAll()
		return nil, err
	}
	if pipes.stderrRead, pipes.stderrWrite, err = os.Pipe(); err != nil {
		pipes.CloseAll()
		return nil, err
	}
	return pipes, nil
}

// CloseChildEnds 关闭父进程侧持有的子端 fd。必须在 Start 之后调用,否则读端
// 永远等不到 EOF。
func (p *stdioPipes) CloseChildEnds() {
	closeFile(p.stdinRead)
	closeFile(p.stdoutWrite)
	closeFile(p.stderrWrite)
}

// CloseAll 关闭全部 fd;重复关闭是安全的。
func (p *stdioPipes) CloseAll() {
	for _, file := range []*os.File{
		p.stdinRead, p.stdinWrite, p.stdoutRead, p.stdoutWrite, p.stderrRead, p.stderrWrite,
	} {
		closeFile(file)
	}
}

// AwaitDrain 等待三个 stdio 协程结束。超过 drainDeadline 仍未结束时,给读端设一个
// 已过期的读超时把它们踢出阻塞状态(比关闭正在被读的 fd 安全)。
func (p *stdioPipes) AwaitDrain(done ...<-chan struct{}) {
	timer := time.NewTimer(drainDeadline)
	defer timer.Stop()

	pending := make([]<-chan struct{}, 0, len(done))
	for _, channel := range done {
		select {
		case <-channel:
		case <-timer.C:
			pending = append(pending, channel)
		}
	}
	if len(pending) == 0 {
		return
	}
	expired := time.Now().Add(-time.Second)
	_ = p.stdoutRead.SetReadDeadline(expired)
	_ = p.stderrRead.SetReadDeadline(expired)
	_ = p.stdinWrite.SetWriteDeadline(expired)
	for _, channel := range pending {
		<-channel
	}
}

// drainInto 把 reader 全部读入 sink,并在结束后关闭 reader。
func drainInto(sink boundedSink, reader *os.File) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reader.Close()
		// sink 的 Write 永不返回错误,因此这里只会因读端结束而停止。
		_, _ = io.Copy(sink, reader)
	}()
	return done
}

func closeFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
