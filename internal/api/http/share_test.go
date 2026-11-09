package http_test

// 团队分享端到端（真库）：管理端签发（token+URL）→ 公开视图脱敏 →
// 公开触发（stub 派发器秒完成）→ 公开轮询输出 → 越权 404 → 撤销即时
// 失效 → 再签发即轮换。公开请求用无会话客户端（token 即凭证）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// pub 请求公开分享 API（无 cookie/CSRF——访客形态）。
func pub(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request) // 无 cookie jar = 访客
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

func TestShareFlowEndToEnd(t *testing.T) {
	fixture := setupAdmin(t)
	teamID := "it-share-flow-" + fixture.suffix

	// 步骤按岗位解析执行者：先造一个聘任 FUNDAMENTAL_ANALYST 的启用智能体。
	status, envelope := fixture.do(t, "POST", "/agents", map[string]any{
		"slug": "it-share-agent-" + fixture.suffix, "name": "分享执行者",
		"executor_kind": "pi", "execution_mode": "local",
		"positions":      []string{"FUNDAMENTAL_ANALYST"},
		"signing_secret": "share-agent-secret-" + fixture.suffix,
		"enabled":        true,
	}, nil)
	if status != 200 {
		t.Fatalf("create agent = %d %v", status, envelope)
	}
	t.Cleanup(func() { _, _ = fixture.do(t, "DELETE", "/agents/it-share-agent-"+fixture.suffix, nil, nil) })

	status, envelope = fixture.do(t, "POST", "/teams", map[string]any{
		"id": teamID, "name": "分享链路", "enabled": true,
		"steps": []map[string]any{
			{"id": "step-1", "target": map[string]string{"position_code": "FUNDAMENTAL_ANALYST"}},
		},
	}, nil)
	if status != 200 {
		t.Fatalf("create team = %d %v", status, envelope)
	}
	t.Cleanup(func() { _, _ = fixture.do(t, "DELETE", "/teams/"+teamID, nil, nil) })

	status, envelope = fixture.do(t, "POST", "/teams/"+teamID+"/share", nil, nil)
	if status != 201 {
		t.Fatalf("issue share = %d %v", status, envelope)
	}
	shareData := envelope["data"].(map[string]any)
	token := shareData["token"].(string)
	if url := shareData["url"].(string); len(url) < len(token) || url[len(url)-len(token):] != token {
		t.Errorf("share url %q must embed token %q", url, token)
	}
	base := fixture.server.URL + "/api/share/v1"

	// 公开视图：脱敏（只有岗位标签）。
	code, view := pub(t, "GET", base+"/"+token, nil)
	if code != 200 {
		t.Fatalf("public view = %d %v", code, view)
	}
	viewData := view["data"].(map[string]any)
	if viewData["name"] != "分享链路" {
		t.Errorf("view name = %v", viewData["name"])
	}
	raw, _ := json.Marshal(viewData)
	if bytes.Contains(raw, []byte("agent")) {
		t.Errorf("view must not leak agent bindings: %s", raw)
	}

	// 公开触发 + 轮询到终态。
	code, runResp := pub(t, "POST", base+"/"+token+"/runs",
		map[string]any{"input": map[string]any{"topic": "e2e"}})
	if code != 202 {
		t.Fatalf("public trigger = %d %v", code, runResp)
	}
	runID := runResp["data"].(map[string]any)["id"].(string)
	var final map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, final = pub(t, "GET", base+"/"+token+"/runs/"+runID, nil)
		if code == 200 {
			if status := final["data"].(map[string]any)["status"]; status != "queued" && status != "running" {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	runData := final["data"].(map[string]any)
	if runData["status"] != "completed" {
		t.Fatalf("public run = %v", runData)
	}

	// 坏 token 与跨 token 轮询一律 404（不区分原因）。
	if code, _ := pub(t, "GET", base+"/sh_bogus-token-000000000000000000", nil); code != 404 {
		t.Errorf("bogus token = %d", code)
	}
	if code, _ := pub(t, "GET", fmt.Sprintf("%s/%s/runs/%s", base, "sh_other-0000000000000000000000", runID), nil); code != 404 {
		t.Errorf("cross-token run = %d, want 404", code)
	}

	// 撤销 → 公开视图与触发同时失效；再签发即轮换。
	if status, _ := fixture.do(t, "DELETE", "/teams/"+teamID+"/share", nil, nil); status != 204 {
		t.Fatalf("revoke = %d", status)
	}
	if code, _ := pub(t, "GET", base+"/"+token, nil); code != 404 {
		t.Errorf("revoked view = %d", code)
	}
	if code, _ := pub(t, "POST", base+"/"+token+"/runs", map[string]any{"input": map[string]any{}}); code != 404 {
		t.Errorf("revoked trigger = %d", code)
	}
	_, envelope = fixture.do(t, "POST", "/teams/"+teamID+"/share", nil, nil)
	newToken := envelope["data"].(map[string]any)["token"].(string)
	if newToken == token {
		t.Error("re-issue must rotate the token")
	}
}
