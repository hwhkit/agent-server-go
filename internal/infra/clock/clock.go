// Package clock 提供时钟实现。消费方（app 服务）按 Go 惯例自行声明
// `interface{ Now() time.Time }` 小接口，本包只提供实现，不定义端口。
package clock

import "time"

// System 是生产时钟，统一返回 UTC。
type System struct{}

// Now 返回当前 UTC 时间。
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed 是测试用固定时钟。
type Fixed struct{ T time.Time }

// Now 返回固定时刻（UTC 归一）。
func (f Fixed) Now() time.Time { return f.T.UTC() }
