package http

// A2A(Agent2Agent) Protocol v1.0 接入层（自旧栈 a2a_router.go 迁移，
// 用例在 app/a2a，协议类型在 domain/platform）。
//
// 设计取舍与旧栈一致：
//   - 只实现 v1.0（PascalCase 方法名 SendMessage/GetTask）；同一份 v1.0 card
//     同时挂 /.well-known/agent-card.json 与 /.well-known/agent.json
//     （pre-1.0 SDK 仍先找后者）。
//   - 路径分片：每个 Agent 有独立 JSON-RPC 端点 {base_path}/{slug} 与独立
//     card {base_path}/{slug}/agent-card.json；root well-known 指向默认 Agent。
//   - 传输层错误（认证失败、body 非法）用 HTTP 状态码表达；协议层错误一律
//     HTTP 200 + JSON-RPC error 信封。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ir-agent-server/internal/app/a2a"
	"ir-agent-server/internal/domain/agent"
	"ir-agent-server/internal/domain/platform"
)

// A2A well-known 发现路径。
const (
	A2AWellKnownCardPath       = "/.well-known/agent-card.json"
	A2ALegacyWellKnownCardPath = "/.well-known/agent.json"
)

// A2ADeps 是 A2A 路由的装配依赖。
type A2ADeps struct {
	Service *a2a.Service
	// Agents 按 slug 解析智能体（domain 仓储结构性满足）。
	Agents interface {
		GetBySlug(ctx context.Context, slug string) (*agent.Agent, error)
	}
	Card                a2a.CardConfig
	RequireBearerAuth   bool
	MaxRequestBodyBytes int
	DefaultCardSlug     string
	PublicBaseURL       string
}

// MountA2A 把 A2A 的具体路径直接注册到父路由上（well-known 与
// {base_path}/{slug} 都是精确模式，可与 "/" 兜底挂载共存）。调用方负责在
// A2A 未启用时不挂载。
func MountA2A(router chi.Router, deps A2ADeps) {
	h := &a2aHandler{deps: deps}
	cardPath := deps.Card.BasePath + "/{slug}/agent-card.json"
	rpcPath := deps.Card.BasePath + "/{slug}"

	router.Get(A2AWellKnownCardPath, h.defaultCard)
	router.Get(A2ALegacyWellKnownCardPath, h.defaultCard)
	router.Get(cardPath, h.agentCard)
	router.Post(rpcPath, h.rpc)
}

// A2ARouter 组装独立的 A2A 子路由（测试与嵌入场景）。
func A2ARouter(deps A2ADeps) chi.Router {
	router := chi.NewRouter()
	MountA2A(router, deps)
	return router
}

type a2aHandler struct{ deps A2ADeps }

func (h *a2aHandler) defaultCard(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(h.deps.DefaultCardSlug))
	if slug == "" {
		writeA2ACardError(w, http.StatusNotFound,
			"no default Agent is configured for A2A discovery; "+
				"set [a2a].default_card_slug or [ir].default_agent_slug, "+
				"or fetch the per-Agent card at {base_path}/{slug}/agent-card.json")
		return
	}
	h.writeCard(w, r, slug)
}

func (h *a2aHandler) agentCard(w http.ResponseWriter, r *http.Request) {
	h.writeCard(w, r, strings.ToLower(chi.URLParam(r, "slug")))
}

func (h *a2aHandler) writeCard(w http.ResponseWriter, r *http.Request, slug string) {
	resolved, ok := h.resolveEnabled(w, r, slug)
	if !ok {
		return
	}
	card := a2a.A2ABuildCard(resolved, h.deps.Card, h.deps.PublicBaseURL)
	body, err := json.Marshal(card)
	if err != nil {
		slog.Error("serialize A2A agent card", "slug", slug, "error", err)
		writeA2ACardError(w, http.StatusInternalServerError, "failed to build agent card")
		return
	}
	w.Header().Set("Content-Type", platform.A2AMediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *a2aHandler) rpc(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(chi.URLParam(r, "slug"))
	resolved, ok := h.resolveEnabled(w, r, slug)
	if !ok {
		return
	}

	// 认证先于一切协议处理：未授权不得暴露 Agent 是否存在之外的信息，
	// 更不能触发执行。
	if h.deps.RequireBearerAuth {
		switch a2a.A2AVerifyAuth(resolved, r.Header.Get("Authorization")) {
		case a2a.A2AAuthOK:
		case a2a.A2AAuthMissing:
			writeA2AUnauthorized(w, "missing bearer credentials")
			return
		case a2a.A2AAuthInvalid:
			slog.Warn("A2A request rejected: invalid bearer token", "slug", slug)
			writeA2AUnauthorized(w, "invalid bearer credentials")
			return
		}
	}

	// A2A-Version 协商（§3.6）：不带按当前版本处理，带了但主版本不同则拒绝。
	if requested := strings.TrimSpace(r.Header.Get("A2A-Version")); requested != "" {
		if !a2aVersionIsSupported(requested) {
			writeA2ARPCError(w, nil, platform.A2AErrVersionNotSupported.WithMetadata(map[string]any{
				"requested": requested,
				"supported": platform.A2AProtocolVersion,
			}))
			return
		}
	}

	limit := int64(h.deps.MaxRequestBodyBytes)
	if limit <= 0 {
		limit = 1024 * 1024
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = r.Body.Close()
	if err != nil || int64(len(body)) > limit {
		writeA2ARPCError(w, nil, platform.A2AErrJSONParse.WithMetadata(map[string]any{
			"detail": "failed to read request body",
		}))
		return
	}

	var request platform.A2AJSONRPCRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeA2ARPCError(w, nil, platform.A2AErrJSONParse.WithMetadata(map[string]any{
			"detail": err.Error(),
		}))
		return
	}
	// id 必须在解析出来之后才能回填到错误响应里。
	id := request.ID
	if request.JSONRPC != "2.0" {
		writeA2ARPCError(w, id, platform.A2AErrInvalidRequest.WithMetadata(map[string]any{
			"detail": `jsonrpc must be "2.0"`,
		}))
		return
	}
	if strings.TrimSpace(request.Method) == "" {
		writeA2ARPCError(w, id, platform.A2AErrInvalidRequest.WithMetadata(map[string]any{
			"detail": "method is required",
		}))
		return
	}

	result, rpcErr := h.deps.Service.Dispatch(r.Context(), resolved, &request)
	if rpcErr != nil {
		writeA2ARPCError(w, id, rpcErr)
		return
	}
	writeA2ARPCSuccess(w, id, result)
}

// resolveEnabled 解析并校验智能体存在且启用；失败时已写好 404。
func (h *a2aHandler) resolveEnabled(w http.ResponseWriter, r *http.Request, slug string) (*agent.ResolvedConfig, bool) {
	stored, err := h.deps.Agents.GetBySlug(r.Context(), slug)
	// 停用的 Agent 不对外暴露能力，与 /agent/{slug} 的行为保持一致。
	if err != nil || stored == nil || !stored.Enabled {
		writeA2ACardError(w, http.StatusNotFound, "Agent not found")
		return nil, false
	}
	return stored.Resolved(), true
}

// a2aVersionIsSupported 只支持同主版本。"1.0" / "1" 放行，"0.3" 拒绝。
func a2aVersionIsSupported(requested string) bool {
	major := func(value string) (int, bool) {
		part, _, _ := strings.Cut(value, ".")
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	left, leftOK := major(requested)
	right, rightOK := major(platform.A2AProtocolVersion)
	return leftOK && rightOK && left == right
}

func writeA2ARPCSuccess(w http.ResponseWriter, id json.RawMessage, result json.RawMessage) {
	response := platform.A2AJSONRPCResponse{JSONRPC: "2.0", ID: a2aOrNull(id), Result: result}
	writeJSON(w, http.StatusOK, response)
}

func writeA2ARPCError(w http.ResponseWriter, id json.RawMessage, rpcErr *platform.A2AJSONRPCError) {
	response := platform.A2AJSONRPCResponse{JSONRPC: "2.0", ID: a2aOrNull(id), Error: rpcErr}
	writeJSON(w, http.StatusOK, response)
}

func a2aOrNull(id json.RawMessage) json.RawMessage {
	if id == nil {
		return platform.A2AJSONRPCNull
	}
	return id
}

// writeA2AUnauthorized 401 必须带 WWW-Authenticate，客户端才知道该用哪种
// 方案（RFC 9110 §11.6.1）。
func writeA2AUnauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    401,
			"status":  "UNAUTHENTICATED",
			"message": detail,
			"details": []map[string]any{{
				"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
				"reason": "UNAUTHENTICATED",
				"domain": platform.A2AErrorDomain,
			}},
		},
	})
	_, _ = w.Write(body)
}

// writeA2ACardError：Agent Card 是普通 HTTP 资源（不是 JSON-RPC），错误用
// HTTP 状态码表达。
func writeA2ACardError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": status, "message": detail},
	})
	_, _ = w.Write(body)
}
