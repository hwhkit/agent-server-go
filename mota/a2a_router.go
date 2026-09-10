package mota

// A2A(Agent2Agent)Protocol v1.0 接入层。对应 rs 侧 src/a2a/mod.rs。
//
// 设计取舍:
//
//   - 只实现 v1.0。v0.3 与 v1.0 在 Part 判别、枚举取值、流事件包装、
//     Agent Card 结构四处都不兼容,双版本会让每处都分叉。唯一的向下兼容
//     动作是把同一份 v1.0 card 同时挂在 /.well-known/agent-card.json
//     (v1.0 路径)和 /.well-known/agent.json(pre-1.0 SDK 仍先找这里)。
//   - 路径分片。本服务托管多个 Agent,而 Agent Card 是一个 agent 一张。
//     每个 Agent 有独立的 JSON-RPC 端点 {base_path}/{slug} 和独立 card
//     {base_path}/{slug}/agent-card.json,与既有 /agent/{slug} 对称,
//     per-Agent 的密钥、超时、并发语义得以保留。root well-known 路径按
//     RFC 8615 指向默认 Agent。
//   - skills 走静态映射,不新增数据库列。

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// A2AWellKnownCardPath 是 v1.0 规定的发现路径。
const A2AWellKnownCardPath = "/.well-known/agent-card.json"

// A2ALegacyWellKnownCardPath 是 pre-1.0 SDK 仍会先找的路径;挂同一份 v1.0
// card 作为别名。
const A2ALegacyWellKnownCardPath = "/.well-known/agent.json"

// A2aState 是 A2A 路由所需状态。刻意只依赖 registry / dispatcher / 任务
// 存储,而不是整个 PlatformState,这样协议层可以配内存后端在无 PostgreSQL
// 环境下测试。
type A2aState struct {
	Config        *A2AConfig
	PublicBaseURL string
	Registry      *AgentRegistry
	dispatcher    *AgentDispatcher
	tasks         *A2ATaskStore
}

// NewA2aState 组装 A2A 状态。
func NewA2aState(
	config *A2AConfig, publicBaseURL string, registry *AgentRegistry,
	dispatcher *AgentDispatcher, tasks *A2ATaskStore,
) *A2aState {
	return &A2aState{
		Config: config, PublicBaseURL: publicBaseURL,
		Registry: registry, dispatcher: dispatcher, tasks: tasks,
	}
}

// NewA2aStateFromPlatform 从共享平台状态组装 A2A 状态。
//
// A2A 走独立的 AgentDispatcher:三层 semaphore 限流与 deadline 递减都在
// 里面,不要在入口层再重复获取额度,否则并发额度会被重复占用。slug 已由
// 路径确定,因此不设默认 Agent 回落。A2A 任务存储走 PostgreSQL,复用
// PlatformState.Store。
func NewA2aStateFromPlatform(platform *PlatformState, config *A2AConfig) (*A2aState, error) {
	executor, err := platform.Executor()
	if err != nil {
		return nil, err
	}
	dispatcher := NewAgentDispatcher(platform.Registry, executor, platform.GlobalExecution, "")
	return NewA2aState(
		config, platform.Config.PublicBaseURL, platform.Registry, dispatcher,
		NewA2APostgresTaskStore(platform.Store),
	), nil
}

// A2ARouter 组装 A2A 子路由。调用方负责在 config.Enabled 为 false 时不挂载。
func A2ARouter(state *A2aState) http.Handler {
	r := chi.NewRouter()
	cardPath := state.Config.BasePath + "/{slug}/agent-card.json"
	rpcPath := state.Config.BasePath + "/{slug}"

	r.Get(A2AWellKnownCardPath, a2aDefaultCardHandler(state))
	r.Get(A2ALegacyWellKnownCardPath, a2aDefaultCardHandler(state))
	r.Get(cardPath, a2aAgentCardHandler(state))
	r.Post(rpcPath, a2aRPCHandler(state))
	return r
}

// a2aDefaultCardHandler 是 root well-known card:返回默认 Agent 的 card
// (RFC 8615 要求 well-known 位于 root,而本服务是多 Agent,因此只能指定
// 其中一个)。
func a2aDefaultCardHandler(state *A2aState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := state.Config.DefaultCardSlug
		if slug == "" {
			a2aWriteCardError(w, http.StatusNotFound,
				"no default Agent is configured for A2A discovery; "+
					"set [a2a].default_card_slug or [mota].default_agent_slug, "+
					"or fetch the per-Agent card at {base_path}/{slug}/agent-card.json")
			return
		}
		a2aWriteCardResponse(w, state, slug)
	}
}

// a2aAgentCardHandler 是 per-Agent card。
func a2aAgentCardHandler(state *A2aState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.ToLower(chi.URLParam(r, "slug"))
		a2aWriteCardResponse(w, state, slug)
	}
}

func a2aWriteCardResponse(w http.ResponseWriter, state *A2aState, slug string) {
	runtime := state.Registry.Get(slug)
	// 停用的 Agent 不对外暴露能力,与 /agent/{slug} 的行为保持一致。
	if runtime == nil || !runtime.Config.Enabled {
		a2aWriteCardError(w, http.StatusNotFound, "Agent not found")
		return
	}
	card := A2ABuildCard(runtime.Config, state.Config, state.PublicBaseURL)
	body, err := json.Marshal(card)
	if err != nil {
		slog.Error("serialize A2A agent card", "slug", slug, "error", err)
		a2aWriteCardError(w, http.StatusInternalServerError, "failed to build agent card")
		return
	}
	w.Header().Set("Content-Type", A2AMediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// a2aRPCHandler 是 JSON-RPC 端点。
//
// 传输层错误(认证失败、body 非法)用 HTTP 状态码表达;协议层错误一律
// HTTP 200 + JSON-RPC error 信封。
func a2aRPCHandler(state *A2aState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.ToLower(chi.URLParam(r, "slug"))
		runtime := state.Registry.Get(slug)
		if runtime == nil || !runtime.Config.Enabled {
			a2aWriteCardError(w, http.StatusNotFound, "Agent not found")
			return
		}

		// 认证先于一切协议处理:未授权不得暴露 Agent 是否存在之外的信息,
		// 更不能触发执行。
		if state.Config.RequireBearerAuth {
			authorization := r.Header.Get("Authorization")
			switch A2AVerifyAuth(runtime.Config, authorization) {
			case A2AAuthOK:
			case A2AAuthMissing:
				a2aWriteUnauthorized(w, "missing bearer credentials")
				return
			case A2AAuthInvalid:
				slog.Warn("A2A request rejected: invalid bearer token", "slug", slug)
				a2aWriteUnauthorized(w, "invalid bearer credentials")
				return
			}
		}

		// A2A-Version 协商(§3.6):不带按当前版本处理,带了但主版本不同则拒绝。
		if requested := strings.TrimSpace(r.Header.Get("A2A-Version")); requested != "" {
			if !a2aVersionIsSupported(requested) {
				a2aWriteRPCError(w, nil, A2AErrVersionNotSupported.WithMetadata(map[string]any{
					"requested": requested,
					"supported": A2AProtocolVersion,
				}))
				return
			}
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, int64(state.Config.MaxRequestBodyBytes)+1))
		_ = r.Body.Close()
		if err != nil || int64(len(body)) > int64(state.Config.MaxRequestBodyBytes) {
			a2aWriteRPCError(w, nil, A2AErrJSONParse.WithMetadata(map[string]any{
				"detail": "failed to read request body",
			}))
			return
		}

		var request A2AJSONRPCRequest
		if err := json.Unmarshal(body, &request); err != nil {
			a2aWriteRPCError(w, nil, A2AErrJSONParse.WithMetadata(map[string]any{
				"detail": err.Error(),
			}))
			return
		}
		// id 必须在解析出来之后才能回填到错误响应里。
		id := request.ID
		if request.JSONRPC != "2.0" {
			a2aWriteRPCError(w, id, A2AErrInvalidRequest.WithMetadata(map[string]any{
				"detail": `jsonrpc must be "2.0"`,
			}))
			return
		}
		if strings.TrimSpace(request.Method) == "" {
			a2aWriteRPCError(w, id, A2AErrInvalidRequest.WithMetadata(map[string]any{
				"detail": "method is required",
			}))
			return
		}

		result, rpcErr := A2ADispatch(r.Context(), state, runtime, &request)
		if rpcErr != nil {
			a2aWriteRPCError(w, id, rpcErr)
			return
		}
		a2aWriteRPCSuccess(w, id, result)
	}
}

// a2aVersionIsSupported 只支持同主版本。"1.0" / "1" 放行,"0.3" 拒绝。
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
	right, rightOK := major(A2AProtocolVersion)
	return leftOK && rightOK && left == right
}

func a2aWriteRPCSuccess(w http.ResponseWriter, id json.RawMessage, result json.RawMessage) {
	response := A2AJSONRPCResponse{JSONRPC: "2.0", ID: a2aOrNull(id), Result: result}
	a2aWriteJSON(w, http.StatusOK, response)
}

func a2aWriteRPCError(w http.ResponseWriter, id json.RawMessage, rpcErr *A2AJSONRPCError) {
	response := A2AJSONRPCResponse{JSONRPC: "2.0", ID: a2aOrNull(id), Error: rpcErr}
	a2aWriteJSON(w, http.StatusOK, response)
}

func a2aOrNull(id json.RawMessage) json.RawMessage {
	if id == nil {
		return a2aJSONRPCNull
	}
	return id
}

func a2aWriteJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("serialize A2A JSON-RPC response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// a2aWriteUnauthorized writes 401 必须带 WWW-Authenticate,客户端才知道该用
// 哪种方案(RFC 9110 §11.6.1)。
func a2aWriteUnauthorized(w http.ResponseWriter, detail string) {
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
				"domain": A2AErrorDomain,
			}},
		},
	})
	_, _ = w.Write(body)
}

// a2aWriteCardError: Agent Card 是普通 HTTP 资源(不是 JSON-RPC),错误用
// HTTP 状态码表达。
func a2aWriteCardError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": status, "message": detail},
	})
	_, _ = w.Write(body)
}
