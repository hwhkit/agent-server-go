// Package sse 实现 Server-Sent Events 推送：TeamRun 步骤状态变更实时流。
//
// 端点：GET /api/admin/v2/team-runs/{id}/events
// 客户端用 EventSource 订阅，服务端在步骤状态变化时推送 JSON 事件。
// 终态（completed/failed/canceled）推送后自动关闭流。
package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Event 是一条 SSE 事件。
type Event struct {
	TeamRunID string          `json:"team_run_id"`
	StepID    string          `json:"step_id,omitempty"`
	State     string          `json:"state"`          // pending|dispatched|succeeded|failed|...
	Status    string          `json:"status"`         // TeamRun 整体状态
	Data      json.RawMessage `json:"data,omitempty"` // 步骤输出（终态时）
	Timestamp time.Time       `json:"timestamp"`
}

// Broker 管理 TeamRun 的订阅者。并发安全。
type Broker struct {
	mu       sync.RWMutex
	channels map[string][]chan Event // teamRunID → subscriber channels
}

// NewBroker 创建事件代理。
func NewBroker() *Broker {
	return &Broker{channels: make(map[string][]chan Event)}
}

// Subscribe 为指定 TeamRun 创建事件通道。返回通道和取消函数。
func (b *Broker) Subscribe(teamRunID string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.channels[teamRunID] = append(b.channels[teamRunID], ch)
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.channels[teamRunID]
		for i, s := range subs {
			if s == ch {
				b.channels[teamRunID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		if len(b.channels[teamRunID]) == 0 {
			delete(b.channels, teamRunID)
		}
	}
}

// Publish 向指定 TeamRun 的所有订阅者推送事件。
// 如果 TeamRun 到终态，自动清理所有订阅。
func (b *Broker) Publish(teamRunID string, event Event) {
	b.mu.RLock()
	subs := b.channels[teamRunID]
	b.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default: // 订阅者太慢，丢弃（SSE 语义允许）
		}
	}

	// 终态清理
	if event.Status == "completed" || event.Status == "failed" || event.Status == "canceled" {
		b.mu.Lock()
		for _, ch := range b.channels[teamRunID] {
			close(ch)
		}
		delete(b.channels, teamRunID)
		b.mu.Unlock()
	}
}

// ServeHTTP 处理 SSE 请求（作为 chi handler 使用）。
// 需要 teamRunID 从 URL 路径参数获取。
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	teamRunID := r.PathValue("id")
	if teamRunID == "" {
		http.Error(w, `{"error":{"code":"invalid_request","message":"team run id is required"}}`, http.StatusBadRequest)
		return
	}

	// SSE 头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Nginx 不缓冲
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"code":"streaming_unsupported","message":"streaming not supported"}}`, http.StatusInternalServerError)
		return
	}

	events, cancel := b.Subscribe(teamRunID)
	defer cancel()

	// 发送初始 ping（确认连接建立）
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("SSE client disconnected", "team_run", teamRunID)
			return
		case event, ok := <-events:
			if !ok {
				return // 通道关闭（终态）
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.State, data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
