package execprocess

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// osStat 便于在测试里读取文件大小。
var osStat = os.Stat

// shellCommand 构造一个 /bin/sh -c 子进程。
func shellCommand(script string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", script)
}

func asExecutionError(t *testing.T, err error) *Error {
	t.Helper()
	var execErr *Error
	if !errors.As(err, &execErr) {
		t.Fatalf("expected an *Error, got %T: %v", err, err)
	}
	return execErr
}

// ---------------------------------------------------------------------------
// 有界捕获(对应 rs 的两个单元测试)
// ---------------------------------------------------------------------------

// 对应 rs prefix_capture_drains_but_retains_only_the_limit。
func TestPrefixSinkDrainsButRetainsOnlyTheLimit(t *testing.T) {
	sink := newPrefixSink(4)
	n, err := sink.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("Write = (%d, %v), want (8, nil) so the copier keeps draining", n, err)
	}
	if got := string(sink.Bytes()); got != "abcd" {
		t.Errorf("Bytes = %q, want abcd", got)
	}
	if !sink.Truncated() {
		t.Error("Truncated should be true")
	}
}

// 分多次写入时前缀边界仍然正确。
func TestPrefixSinkAcrossMultipleWrites(t *testing.T) {
	sink := newPrefixSink(5)
	for _, chunk := range []string{"ab", "cd", "ef", "gh"} {
		if _, err := sink.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(sink.Bytes()); got != "abcde" {
		t.Errorf("Bytes = %q, want abcde", got)
	}
	if !sink.Truncated() {
		t.Error("Truncated should be true")
	}
}

// 未超限时不报截断。
func TestPrefixSinkExactLimitIsNotTruncated(t *testing.T) {
	sink := newPrefixSink(4)
	if _, err := sink.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if sink.Truncated() {
		t.Error("writing exactly the limit must not count as truncated")
	}
	if got := string(sink.Bytes()); got != "abcd" {
		t.Errorf("Bytes = %q, want abcd", got)
	}
}

// 对应 rs tail_capture_keeps_the_final_jsonl_event。
func TestTailSinkKeepsTheFinalJSONLEvent(t *testing.T) {
	const finalEvent = "{\"type\":\"message_end\",\"final\":true}\n"
	sink := newTailSink(128)
	if _, err := sink.Write([]byte(strings.Repeat("x", 8192))); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("\n" + finalEvent)); err != nil {
		t.Fatal(err)
	}
	if !sink.Truncated() {
		t.Error("Truncated should be true")
	}
	got := sink.Bytes()
	if len(got) != 128 {
		t.Errorf("len = %d, want 128", len(got))
	}
	if !strings.HasSuffix(string(got), finalEvent) {
		t.Errorf("tail must end with the final event, got %q", got)
	}
}

// 环形缓冲跨越回绕点后仍按时间顺序输出。
func TestTailSinkRotatesRingIntoChronologicalOrder(t *testing.T) {
	sink := newTailSink(5)
	for _, chunk := range []string{"abc", "de", "fg"} {
		if _, err := sink.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(sink.Bytes()); got != "cdefg" {
		t.Errorf("Bytes = %q, want cdefg", got)
	}
}

// 未填满时只返回已写入的部分,且不报截断。
func TestTailSinkUnderfilled(t *testing.T) {
	sink := newTailSink(16)
	if _, err := sink.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := string(sink.Bytes()); got != "abc" {
		t.Errorf("Bytes = %q, want abc", got)
	}
	if sink.Truncated() {
		t.Error("Truncated should be false")
	}
}

// limit 为 0 时排空但不保留任何字节。
func TestTailSinkZeroLimitDrainsEverything(t *testing.T) {
	sink := newTailSink(0)
	if _, err := sink.Write([]byte("anything")); err != nil {
		t.Fatal(err)
	}
	if len(sink.Bytes()) != 0 {
		t.Errorf("Bytes = %q, want empty", sink.Bytes())
	}
	if !sink.Truncated() {
		t.Error("any byte beyond a zero limit counts as truncated")
	}
}

// 单次写入超过整个窗口时取其尾部。
func TestTailSinkSingleOversizedWrite(t *testing.T) {
	sink := newTailSink(4)
	if _, err := sink.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if got := string(sink.Bytes()); got != "efgh" {
		t.Errorf("Bytes = %q, want efgh", got)
	}
}

func TestLossyUTF8(t *testing.T) {
	if got := LossyUTF8([]byte("正常文本")); got != "正常文本" {
		t.Errorf("valid UTF-8 must pass through, got %q", got)
	}
	got := LossyUTF8([]byte{'a', 0xff, 'b'})
	if !strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("invalid bytes must become U+FFFD, got %q", got)
	}
	if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "b") {
		t.Errorf("surrounding valid bytes must survive, got %q", got)
	}
}

// stdout 与 stderr 同时被逃逸进程持有时，两条读流必须共用一次排水截止时间。
// 旧实现依次消费同一个 timer.C：stdout 消费唯一的超时事件后，stderr 会永久等待。
func TestStdioPipesAwaitDrainUnblocksBothHeldOutputStreams(t *testing.T) {
	pipes, err := newStdioPipes()
	if err != nil {
		t.Fatalf("newStdioPipes: %v", err)
	}
	defer pipes.CloseAll()

	stdoutDone := drainInto(newPrefixSink(16), pipes.stdoutRead)
	stderrDone := drainInto(newPrefixSink(16), pipes.stderrRead)
	awaitDone := make(chan struct{})
	go func() {
		pipes.AwaitDrain(stdoutDone, stderrDone)
		close(awaitDone)
	}()

	select {
	case <-awaitDone:
	case <-time.After(drainDeadline + 2*time.Second):
		// 先释放仍被测试持有的写端，避免失败路径遗留读协程。
		pipes.CloseAll()
		<-awaitDone
		t.Fatal("AwaitDrain blocked after both held output streams reached the shared deadline")
	}

	for name, done := range map[string]<-chan struct{}{
		"stdout": stdoutDone,
		"stderr": stderrDone,
	} {
		select {
		case <-done:
		default:
			t.Errorf("%s drain was not released", name)
		}
	}
}

// ---------------------------------------------------------------------------
// RunCommand:真实子进程
// ---------------------------------------------------------------------------

func TestRunCommandCapturesStdout(t *testing.T) {
	stdout, err := RunCommand(context.Background(), shellCommand(`printf 'hello world'`),
		nil, 5*time.Second, 1024, CaptureComplete, "test")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if string(stdout) != "hello world" {
		t.Errorf("stdout = %q, want 'hello world'", stdout)
	}
}

// stdin payload 写完后必须被关闭,否则读 stdin 的子进程永不退出。
func TestRunCommandWritesAndClosesStdin(t *testing.T) {
	stdout, err := RunCommand(context.Background(), shellCommand(`cat`),
		[]byte("piped payload"), 5*time.Second, 1024, CaptureComplete, "test")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if string(stdout) != "piped payload" {
		t.Errorf("stdout = %q, want 'piped payload'", stdout)
	}
}

// 无 payload 时 stdin 依然要立即 EOF。
func TestRunCommandClosesStdinWithoutPayload(t *testing.T) {
	stdout, err := RunCommand(context.Background(), shellCommand(`cat; printf 'done'`),
		nil, 5*time.Second, 1024, CaptureComplete, "test")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if string(stdout) != "done" {
		t.Errorf("stdout = %q, want 'done'", stdout)
	}
}

// 子进程不读 stdin 也不能让父进程报错(EPIPE 必须被忽略)。
func TestRunCommandToleratesUnreadStdin(t *testing.T) {
	payload := []byte(strings.Repeat("x", 512*1024))
	stdout, err := RunCommand(context.Background(), shellCommand(`printf 'ignored stdin'`),
		payload, 5*time.Second, 1024, CaptureComplete, "test")
	if err != nil {
		t.Fatalf("a child that ignores stdin must not fail the run: %v", err)
	}
	if string(stdout) != "ignored stdin" {
		t.Errorf("stdout = %q", stdout)
	}
}

// 非零退出码 → executor_failed,并附上审计快照。
func TestRunCommandNonZeroExitIsExecutorFailed(t *testing.T) {
	_, err := RunCommand(context.Background(), shellCommand(`printf 'partial'; printf 'oops' >&2; exit 3`),
		nil, 5*time.Second, 1024, CaptureComplete, "test")
	execErr := asExecutionError(t, err)
	if execErr.Code != "executor_failed" {
		t.Fatalf("code = %q, want executor_failed", execErr.Code)
	}
	if execErr.PublicMessage() != "agent execution failed" {
		t.Errorf("public message = %q", execErr.PublicMessage())
	}
	raw := execErr.ExecutorResultJSON()
	if raw == nil {
		t.Fatal("a failed exit must attach the captured output")
	}
	var captured map[string]any
	if err := json.Unmarshal([]byte(*raw), &captured); err != nil {
		t.Fatalf("captured output is not JSON: %v", err)
	}
	for _, key := range []string{"stdout", "stderr", "stdoutTruncated", "stderrTruncated"} {
		if _, present := captured[key]; !present {
			t.Errorf("captured output missing %q: %s", key, *raw)
		}
	}
	if captured["stdout"] != "partial" {
		t.Errorf("captured stdout = %v, want partial", captured["stdout"])
	}
	if captured["stderr"] != "oops" {
		t.Errorf("captured stderr = %v, want oops", captured["stderr"])
	}
}

// 启动不存在的二进制 → executor_unavailable。
func TestRunCommandMissingBinaryIsExecutorUnavailable(t *testing.T) {
	_, err := RunCommand(context.Background(), exec.Command("/nonexistent/ir-agent-binary"),
		nil, 5*time.Second, 1024, CaptureComplete, "missing")
	execErr := asExecutionError(t, err)
	if execErr.Code != "executor_unavailable" {
		t.Fatalf("code = %q, want executor_unavailable", execErr.Code)
	}
	if !strings.Contains(execErr.Detail, "start missing") {
		t.Errorf("detail should name the label: %q", execErr.Detail)
	}
	if execErr.PublicMessage() != "agent executor is unavailable" {
		t.Errorf("public message = %q", execErr.PublicMessage())
	}
}

// Complete 模式下 stdout 超限 → output_too_large。
func TestRunCommandCompleteCaptureRejectsOversizedStdout(t *testing.T) {
	_, err := RunCommand(context.Background(), shellCommand(`printf 'aaaaaaaaaaaaaaaaaaaa'`),
		nil, 5*time.Second, 4, CaptureComplete, "test")
	execErr := asExecutionError(t, err)
	if execErr.Code != "output_too_large" {
		t.Fatalf("code = %q, want output_too_large", execErr.Code)
	}
	if execErr.PublicMessage() != "agent output exceeded the configured limit" {
		t.Errorf("public message = %q", execErr.PublicMessage())
	}
	if execErr.ExecutorResultJSON() == nil {
		t.Error("oversized output must still attach the captured snapshot")
	}
}

// EventStreamTail 模式下超限只降级为尾部保留,不报错。
func TestRunCommandTailCaptureToleratesOversizedStream(t *testing.T) {
	script := `for i in $(seq 1 200); do printf '{"type":"noise","i":%d}\n' "$i"; done
printf '{"type":"message_end"}\n'`
	stdout, err := RunCommand(context.Background(), shellCommand(script), nil, 10*time.Second, 128, CaptureEventStreamTail, "test")
	if err != nil {
		t.Fatalf("tail capture must tolerate oversized streams: %v", err)
	}
	if len(stdout) != 128 {
		t.Errorf("len = %d, want the bounded tail of 128", len(stdout))
	}
	if !strings.Contains(string(stdout), `{"type":"message_end"}`) {
		t.Errorf("the final event must survive in the tail: %q", stdout)
	}
}

// stderr 被排空但不影响 stdout 解析。
func TestRunCommandDrainsStderrWithoutFailing(t *testing.T) {
	script := `for i in $(seq 1 500); do printf 'warning line %d\n' "$i" >&2; done
printf 'clean result'`
	stdout, err := RunCommand(context.Background(), shellCommand(script), nil, 10*time.Second, 64, CaptureComplete, "test")
	if err != nil {
		t.Fatalf("verbose stderr must not fail the run: %v", err)
	}
	if string(stdout) != "clean result" {
		t.Errorf("stdout = %q, want 'clean result'", stdout)
	}
}

// ---------------------------------------------------------------------------
// 超时与进程组清理
// ---------------------------------------------------------------------------

// 超时 → execution_timeout,且不会久等。
func TestRunCommandTimeoutIsExecutionTimeout(t *testing.T) {
	started := time.Now()
	_, err := RunCommand(context.Background(), shellCommand(`sleep 30`),
		nil, 200*time.Millisecond, 1024, CaptureComplete, "slow")
	elapsed := time.Since(started)

	execErr := asExecutionError(t, err)
	if execErr.Code != "execution_timeout" {
		t.Fatalf("code = %q, want execution_timeout", execErr.Code)
	}
	if !strings.Contains(execErr.Detail, "slow exceeded its deadline") {
		t.Errorf("detail = %q", execErr.Detail)
	}
	if execErr.PublicMessage() != "agent execution timed out" {
		t.Errorf("public message = %q", execErr.PublicMessage())
	}
	// SIGTERM 就能杀掉 sleep,不该走完 2s 宽限。
	if elapsed > 2*time.Second {
		t.Errorf("timeout handling took %s, expected a prompt SIGTERM kill", elapsed)
	}
}

// 忽略 SIGTERM 的子进程会在宽限期后被 SIGKILL。
func TestRunCommandTimeoutEscalatesToSIGKILL(t *testing.T) {
	// trap '' TERM 让 shell 忽略 SIGTERM。
	_, err := RunCommand(context.Background(), shellCommand(`trap '' TERM; sleep 30`),
		nil, 200*time.Millisecond, 1024, CaptureComplete, "stubborn")
	execErr := asExecutionError(t, err)
	if execErr.Code != "execution_timeout" {
		t.Fatalf("code = %q, want execution_timeout", execErr.Code)
	}
}

// 超时清理必须覆盖子进程 fork 出的后代(进程组语义)。
func TestRunCommandTimeoutKillsGrandchildren(t *testing.T) {
	marker := t.TempDir() + "/grandchild-alive"
	// 孙进程在父进程被杀后仍会持续写文件,除非整个进程组被清掉。
	script := `( while true; do printf 'x' >> ` + marker + `; sleep 0.05; done ) &
sleep 30`
	_, err := RunCommand(context.Background(), shellCommand(script), nil, 300*time.Millisecond, 1024, CaptureComplete, "tree")
	if execErr := asExecutionError(t, err); execErr.Code != "execution_timeout" {
		t.Fatalf("code = %q, want execution_timeout", execErr.Code)
	}

	// 清理后孙进程不应再增长文件。
	time.Sleep(300 * time.Millisecond)
	first := fileSizeOrZero(marker)
	time.Sleep(400 * time.Millisecond)
	second := fileSizeOrZero(marker)
	if second != first {
		t.Errorf("grandchild still running after cleanup: size grew %d -> %d", first, second)
	}
}

// 父请求取消必须像本地 timeout 一样终止整个进程组，而不只是让调用方返回。
func TestRunCommandParentCancellationKillsGrandchildren(t *testing.T) {
	directory := t.TempDir()
	started := directory + "/started"
	marker := directory + "/grandchild-alive"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := `( while true; do printf 'x' >> ` + marker + `; sleep 0.05; done ) &
printf 'ready' > ` + started + `
sleep 30`
	result := make(chan error, 1)
	go func() {
		_, err := RunCommand(ctx, shellCommand(script), nil, 30*time.Second, 1024, CaptureComplete, "tree")
		result <- err
	}()

	startedDeadline := time.Now().Add(3 * time.Second)
	for fileSizeOrZero(started) == 0 && time.Now().Before(startedDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fileSizeOrZero(started) == 0 {
		cancel()
		<-result
		t.Fatal("child process did not start")
	}

	cancel()
	var runErr error
	select {
	case runErr = <-result:
	case <-time.After(processTerminationGrace + 2*time.Second):
		t.Fatal("RunCommand did not reap the child process after parent cancellation")
	}
	execErr := asExecutionError(t, runErr)
	if execErr.Code != "execution_timeout" {
		t.Fatalf("code = %q, want execution_timeout", execErr.Code)
	}
	if !strings.Contains(execErr.Detail, "parent context") {
		t.Errorf("detail = %q, want parent cancellation context", execErr.Detail)
	}

	// 返回后孙进程必须已停止，不能继续写入活动标记。
	time.Sleep(300 * time.Millisecond)
	first := fileSizeOrZero(marker)
	time.Sleep(400 * time.Millisecond)
	second := fileSizeOrZero(marker)
	if second != first {
		t.Errorf("grandchild still running after parent cancellation: size grew %d -> %d", first, second)
	}
}

// 正常退出后同样要回收仍在后台运行的孙进程。
func TestRunCommandNormalExitKillsGrandchildren(t *testing.T) {
	marker := t.TempDir() + "/lingering"
	script := `( while true; do printf 'x' >> ` + marker + `; sleep 0.05; done ) &
printf 'parent done'`
	stdout, err := RunCommand(context.Background(), shellCommand(script), nil, 5*time.Second, 1024, CaptureComplete, "tree")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if string(stdout) != "parent done" {
		t.Errorf("stdout = %q", stdout)
	}

	time.Sleep(300 * time.Millisecond)
	first := fileSizeOrZero(marker)
	time.Sleep(400 * time.Millisecond)
	second := fileSizeOrZero(marker)
	if second != first {
		t.Errorf("grandchild survived a clean exit: size grew %d -> %d", first, second)
	}
}

func fileSizeOrZero(path string) int64 {
	info, err := osStat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
