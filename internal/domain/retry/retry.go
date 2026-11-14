// Package retry 提供固定次数 + 退避表 + 错误分类的重试循环（domain 层，
// 纯标准库）。语义与 hwhkit-go/retry 模块一致（跨仓库复用走库；core 分层
// 规则要求 domain 只依赖标准库，故 core 内自持同语义实现）。
//
// 抽象自 callback 投递循环：Delays 显式退避表（越界取最后一项），
// Classifier 决定错误是否值得重试，ctx 取消随时终止。
package retry

import (
	"context"
	"time"
)

// Classifier 判定错误类别。
type Classifier interface {
	// Retryable 报告该错误是否值得再次尝试。
	Retryable(err error) bool
}

// ClassifierFunc 让普通函数实现 Classifier。
type ClassifierFunc func(err error) bool

// Retryable 实现 Classifier。
func (f ClassifierFunc) Retryable(err error) bool { return f(err) }

// Config 是重试策略。
type Config struct {
	// Attempts 是总尝试次数（含首次；<=1 视为 1）。
	Attempts int
	// Delays 是显式退避表：第 attempt 次失败后取 Delays[attempt]
	// （越界取最后一项；空表不等待）。
	Delays []time.Duration
	// Classifier 决定错误是否可重试（nil = 全部可重试）。
	Classifier Classifier
}

// Result 汇总一次 Do 的结局。
type Result struct {
	// Err 是最后一次错误（成功时为 nil）。
	Err error
	// Attempts 是实际尝试次数。
	Attempts int
}

// Do 执行 op 并按策略重试。op 返回 nil 即成功。
func Do(ctx context.Context, cfg Config, op func(attempt int) error) Result {
	attempts := cfg.Attempts
	if attempts < 1 {
		attempts = 1
	}
	classifier := cfg.Classifier
	if classifier == nil {
		classifier = ClassifierFunc(func(error) bool { return true })
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{Err: err, Attempts: attempt}
		}
		lastErr = op(attempt)
		if lastErr == nil {
			return Result{Attempts: attempt + 1}
		}
		if !classifier.Retryable(lastErr) || attempt == attempts-1 {
			return Result{Err: lastErr, Attempts: attempt + 1}
		}
		delay := delayFor(cfg.Delays, attempt)
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return Result{Err: ctx.Err(), Attempts: attempt + 1}
		case <-time.After(delay):
		}
	}
	return Result{Err: lastErr, Attempts: attempts}
}

func delayFor(delays []time.Duration, attempt int) time.Duration {
	if len(delays) == 0 {
		return 0
	}
	if attempt >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt]
}
