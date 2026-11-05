// Package http 是接口适配层：把 app 用例服务暴露为 HTTP 端点，并负责
// 请求拆解（头/体/限长）与响应渲染（problem+json、协议错误体、诊断头）。
//
// 线协议形状与旧栈逐字兼容（PUSH-PRODUCTION / AGENT-PULL-API 契约），
// 上游平台与连接器无感知切换。
package http

import "net/http"

// SecurityHeaders 是基础安全响应头中间件。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
