package http_test

// 管理 API v2：执行后端（providers）、账号管理（users）、通道事件
// （channel-events）端点集成测试（真 PostgreSQL，复用 setupAdmin fixture）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"

	"ir-agent-server/internal/domain/audit"
)

// providerCleanup 删除测试写入的 Provider 行（键是全局的，测试间共享库）。
func providerCleanup(t *testing.T, f *adminFixture, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = f.db.Pool.Exec(context.Background(),
				`DELETE FROM platform_settings WHERE key IN ($1, $2)`,
				"executor_provider:"+id, "executor_provider_item_revision:"+id)
		}
	})
}

func findProvider(t *testing.T, f *adminFixture, id string) map[string]any {
	t.Helper()
	status, envelope := f.do(t, "GET", "/providers", nil, nil)
	if status != 200 {
		t.Fatalf("list providers = %d %v", status, envelope)
	}
	for _, item := range envelope["data"].([]any) {
		view := item.(map[string]any)
		if view["id"] == id {
			return view
		}
	}
	t.Fatalf("provider %s missing from list", id)
	return nil
}

func TestAdminProviders(t *testing.T) {
	fixture := setupAdmin(t)
	providerCleanup(t, fixture, "claude_managed_agent")
	const id = "claude_managed_agent"
	apiKey := "sk-cm-" + fixture.suffix

	current := findProvider(t, fixture, id)
	revision := int64(current["revision"].(float64))

	// 管理员保存：configured/secret_configured 翻转，密钥永不回显。
	status, envelope := fixture.do(t, "PUT", "/providers/"+id, map[string]any{
		"enabled": true, "api_key": apiKey,
		"base_url":           "https://api.anthropic.example.test",
		"agent_id":           "agent-" + fixture.suffix,
		"environment_id":     "env-" + fixture.suffix,
		"max_response_bytes": 65536,
	}, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision, 10))})
	if status != 200 {
		t.Fatalf("update provider = %d %v", status, envelope)
	}
	view := dataOf(t, envelope)
	if int64(view["revision"].(float64)) != revision+1 || view["configured"] != true ||
		view["secret_configured"] != true {
		t.Errorf("view = %v", view)
	}
	if raw, _ := json.Marshal(envelope); bytes.Contains(raw, []byte(apiKey)) {
		t.Error("provider response must never echo the api key")
	}

	// 陈旧 If-Match → 412（用合法字段，保证失败发生在 CAS 而不是校验）。
	status, envelope = fixture.do(t, "PUT", "/providers/"+id,
		map[string]any{"enabled": false},
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision, 10))})
	if status != 412 {
		t.Errorf("stale if-match = %d %v", status, envelope)
	}

	// 未知 Provider → 404；非法字段 → 422；非法 If-Match → 400。
	status, _ = fixture.do(t, "PUT", "/providers/no_such_provider",
		map[string]any{}, map[string]string{"If-Match": `"0"`})
	if status != 404 {
		t.Errorf("unknown provider = %d", status)
	}
	status, _ = fixture.do(t, "PUT", "/providers/"+id,
		map[string]any{"bogus_field": 1},
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision+1, 10))})
	if status != 422 {
		t.Errorf("invalid field = %d", status)
	}
	status, _ = fixture.do(t, "PUT", "/providers/"+id,
		map[string]any{}, map[string]string{"If-Match": "abc"})
	if status != 400 {
		t.Errorf("invalid if-match = %d", status)
	}

	// 普通用户：可读但配置脱敏；写 403。
	userFixture := &adminFixture{server: fixture.server, suffix: fixture.suffix,
		base: fixture.base, db: fixture.db}
	jar, _ := cookiejar.New(nil)
	userFixture.client = &http.Client{Jar: jar}
	userFixture.login(t, "user-"+fixture.suffix, "user-pass-"+fixture.suffix)
	status, envelope = userFixture.do(t, "GET", "/providers", nil, nil)
	if status != 200 {
		t.Fatalf("user list providers = %d", status)
	}
	redacted := 0
	for _, item := range envelope["data"].([]any) {
		view := item.(map[string]any)
		config, _ := view["config"].(map[string]any)
		if len(config) == 0 && view["editable"] == false {
			redacted++
		}
	}
	if redacted != 11 {
		t.Errorf("redacted views = %d", redacted)
	}
	status, _ = userFixture.do(t, "PUT", "/providers/"+id,
		map[string]any{"enabled": false}, nil)
	if status != 403 {
		t.Errorf("user write provider = %d", status)
	}

	// 审计已落库（provider.update 可查）。
	status, envelope = fixture.do(t, "GET",
		"/audits?action=provider.update&limit=5", nil, nil)
	if status != 200 || dataOf(t, envelope)["total"].(float64) < 1 {
		t.Errorf("provider audits = %d %v", status, envelope)
	}
}

func TestAdminUsers(t *testing.T) {
	fixture := setupAdmin(t)
	name := "it-user-" + fixture.suffix

	// 列表包含 fixture 账号，且永不携带哈希。
	status, envelope := fixture.do(t, "GET", "/users", nil, nil)
	if status != 200 {
		t.Fatalf("list users = %d", status)
	}
	raw, _ := json.Marshal(envelope)
	if !bytes.Contains(raw, []byte("admin-"+fixture.suffix)) {
		t.Errorf("admin missing from users: %s", raw)
	}
	if bytes.Contains(raw, []byte("$argon2")) {
		t.Error("user list must never carry password hashes")
	}

	// 创建 → 重复 409 → 弱口令 422。
	status, envelope = fixture.do(t, "POST", "/users", map[string]any{
		"username": name, "password": "it-strong-pass-123", "role": "user",
	}, nil)
	if status != 201 {
		t.Fatalf("create user = %d %v", status, envelope)
	}
	status, _ = fixture.do(t, "POST", "/users", map[string]any{
		"username": name, "password": "it-strong-pass-456", "role": "user",
	}, nil)
	if status != 409 {
		t.Errorf("duplicate user = %d", status)
	}
	status, _ = fixture.do(t, "POST", "/users", map[string]any{
		"username": "weak-" + fixture.suffix, "password": "short", "role": "user",
	}, nil)
	if status != 422 {
		t.Errorf("weak password = %d", status)
	}

	// 更新（禁用 + 角色）→ 删除 → 再删 404。
	disabled := false
	status, envelope = fixture.do(t, "PUT", "/users/"+name,
		map[string]any{"enabled": disabled, "role": "admin"}, nil)
	if status != 200 {
		t.Fatalf("update user = %d %v", status, envelope)
	}
	view := dataOf(t, envelope)
	if view["enabled"] != false || view["role"] != "admin" {
		t.Errorf("updated view = %v", view)
	}
	if status, _ := fixture.do(t, "DELETE", "/users/"+name, nil, nil); status != 204 {
		t.Errorf("delete user = %d", status)
	}
	if status, _ := fixture.do(t, "DELETE", "/users/"+name, nil, nil); status != 404 {
		t.Errorf("delete missing user = %d", status)
	}

	// 普通用户访问账号管理 → 403（后端强制，不只是 UI 隐藏）。
	userFixture := &adminFixture{server: fixture.server, suffix: fixture.suffix,
		base: fixture.base, db: fixture.db}
	jar, _ := cookiejar.New(nil)
	userFixture.client = &http.Client{Jar: jar}
	userFixture.login(t, "user-"+fixture.suffix, "user-pass-"+fixture.suffix)
	if status, _ := userFixture.do(t, "GET", "/users", nil, nil); status != 403 {
		t.Errorf("user list users = %d", status)
	}
}

func TestAdminChannelEvents(t *testing.T) {
	fixture := setupAdmin(t)
	slug := "it-chan-" + fixture.suffix

	// 写一条通道事件后可按 slug 过滤读取。
	detail := `{"outcome":"completed"}`
	if err := fixture.events.RecordChannelEvent(context.Background(), audit.ChannelEvent{
		EventType: "callback", AgentSlug: &slug, URL: "https://cb.example.test",
		Detail: &detail,
	}); err != nil {
		t.Fatalf("record channel event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.db.Pool.Exec(context.Background(),
			`DELETE FROM channel_events WHERE agent_slug = $1`, slug)
	})

	status, envelope := fixture.do(t, "GET", "/channel-events?slug="+slug, nil, nil)
	if status != 200 {
		t.Fatalf("list channel events = %d", status)
	}
	items, _ := envelope["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", envelope)
	}
	event := items[0].(map[string]any)
	if event["EventType"] != "callback" {
		t.Errorf("event = %v", event)
	}
	if raw, _ := json.Marshal(event); bytes.Contains(raw, []byte("$argon2")) ||
		strings.Contains(string(raw), "password") {
		t.Errorf("channel event leaks sensitive text: %s", raw)
	}
}
