package audit

import "time"

// ChannelEvent 是通道事件记录（callback 回传 / heartbeat 心跳 / 自检），
// 供管理台运行记录页展示与运维诊断。对应 channel_events 表。
type ChannelEvent struct {
	// ID 与 CreatedAt 由存储填充（写入时忽略）。
	ID        int64
	CreatedAt time.Time
	// EventType 取值如 "callback" / "heartbeat"。
	EventType string
	AgentSlug *string
	RunID     *string
	// URL 是无凭据的 origin 形态（日志/审计不落完整回调地址）。
	URL        string
	StatusCode *int64
	Accepted   *bool
	RunStatus  *string
	// Pass 是成功判定（如心跳：自检通过且 HTTP 2xx）。
	Pass      *bool
	LatencyMs *int64
	Detail    *string
	Input     *string
	Output    *string
}
