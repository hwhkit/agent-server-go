package mota

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testPlatformConfig 返回一份通过校验的平台配置:走 platformConfigFromFile 拿到
// 与生产一致的默认值(含 [gateway]/[local_agents]),只覆盖测试必需的字段。
func testPlatformConfig(t *testing.T) *PlatformConfig {
	t.Helper()
	username := "admin"
	password := "correct horse battery staple"
	secret := strings.Repeat("s", 32)
	ttl := uint64(3600)
	baseURL := "http://127.0.0.1:8080"
	concurrency := 4
	root := &platformRootFile{
		Platform: platformSectionFile{PublicBaseURL: &baseURL, GlobalMaxConcurrency: &concurrency},
		Admin: adminSectionFile{
			Username:       &username,
			Password:       &password,
			SessionSecret:  &secret,
			SessionTTLSecs: &ttl,
		},
	}
	cfg, err := platformConfigFromFile(root)
	if err != nil {
		t.Fatalf("build test platform config: %v", err)
	}
	return cfg
}

func testPlatform(t *testing.T) (*PlatformState, *httptest.Server, *http.Client) {
	t.Helper()
	requireTestDatabase(t)
	cfg := testPlatformConfig(t)
	state, err := NewPlatformState(context.Background(), cfg, PlaceholderProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/", state.Gateway)
	admin := &AdminHandler{State: state}
	mux.Handle("/admin", admin)
	mux.Handle("/api/admin/", admin)
	server := httptest.NewServer(mux)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	t.Cleanup(func() { server.Close(); state.Close() })
	return state, server, client
}
func loginAdmin(t *testing.T, server *httptest.Server, client *http.Client) string {
	body := bytes.NewBufferString(`{"username":"admin","password":"correct horse battery staple"}`)
	resp, err := client.Post(server.URL+"/api/admin/login", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login status=%d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			CSRF string `json:"csrfToken"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Data.CSRF
}
func createTestAgent(t *testing.T, server *httptest.Server, client *http.Client, csrf, slug string) {
	t.Helper()
	// executor_kind=rust 走进程内 Processor(Rust 版命名),不需要外部 CLI 或网关。
	body := map[string]any{"slug": slug, "name": "Research One", "executor_kind": "rust", "execution_mode": "gateway", "signing_secret": "agent-secret-0123456789", "signature_encoding": "hex", "workspace_path": ".", "permission_mode": "dontAsk", "timeout_secs": 30, "max_concurrency": 1, "queue_capacity": 2}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", server.URL+"/api/admin/agents", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		detail, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, detail)
	}
}
func TestPlatformAdminAndGateway(t *testing.T) {
	state, server, client := testPlatform(t)
	page, err := client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(page.Body)
	page.Body.Close()
	// 与 Rust 侧同一份 web/admin.html：必须带智能体中心、岗位配置与托管模式。
	for _, marker := range []string{
		`id="providers-section"`,
		`id="agent-execution-mode"`,
		`<option value="opencode">OpenCode</option>`,
		`<option value="openai_agent">OpenAI 托管智能体</option>`,
		`<h2 id="runs-title" data-i18n="runs.title">运行日志</h2>`,
	} {
		if !strings.Contains(string(pageBody), marker) {
			t.Fatalf("embedded admin console is missing %s", marker)
		}
	}
	if page.StatusCode != 200 {
		t.Fatalf("admin page status=%d", page.StatusCode)
	}
	csrf := loginAdmin(t, server, client)
	providersResponse, err := client.Get(server.URL + "/api/admin/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer providersResponse.Body.Close()
	if providersResponse.StatusCode != http.StatusOK {
		t.Fatalf("providers status=%d", providersResponse.StatusCode)
	}
	var providersEnvelope struct {
		Data []ProviderView `json:"data"`
	}
	if err := json.NewDecoder(providersResponse.Body).Decode(&providersEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(providersEnvelope.Data) != len(providerIDs) {
		t.Fatalf("providers=%d want=%d", len(providersEnvelope.Data), len(providerIDs))
	}
	// PostgreSQL 是跨测试运行共享的,slug / runId / nonce / 幂等键都必须唯一。
	slug := uniqueSlug("research-one")
	createTestAgent(t, server, client, csrf, slug)
	t.Cleanup(func() { _, _ = state.Store.DeleteAgent(context.Background(), slug) })

	runID := unique("run")
	body := []byte(`{"runId":"` + runID + `","params":{"symbols":["AAPL"]}}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := strings.ReplaceAll(unique("nonce"), "-", "_")
	idem := unique("idem")
	canonical := "v1\nPOST\n/agent/" + slug + "\n" + timestamp + "\n" + nonce + "\n" + runID + "\n" + idem + "\n" + bodyHash(body)
	req, _ := http.NewRequest("POST", server.URL+"/agent/"+slug, bytes.NewReader(body))
	req.Header.Set("X-Agent-Timestamp", timestamp)
	req.Header.Set("X-Agent-Nonce", nonce)
	req.Header.Set("X-Agent-Run-Id", runID)
	req.Header.Set("X-Idempotency-Key", idem)
	req.Header.Set("X-Agent-Signature", ComputeSignature("agent-secret-0123456789", []byte(canonical), "hex"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("gateway status=%d body=%s", resp.StatusCode, firstBody)
	}
	if resp.Header.Get("X-Agent-Run-Id") != runID {
		t.Fatal("missing run header")
	}

	replay, _ := http.NewRequest("POST", server.URL+"/agent/"+slug, bytes.NewReader(body))
	replay.Header = req.Header.Clone()
	replayResp, err := client.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	replayResp.Body.Close()
	if replayResp.StatusCode != 401 {
		t.Fatalf("replayed nonce status=%d", replayResp.StatusCode)
	}

	nonce = strings.ReplaceAll(unique("nonce2"), "-", "_")
	canonical = "v1\nPOST\n/agent/" + slug + "\n" + timestamp + "\n" + nonce + "\n" + runID + "\n" + idem + "\n" + bodyHash(body)
	cached, _ := http.NewRequest("POST", server.URL+"/agent/"+slug, bytes.NewReader(body))
	cached.Header = req.Header.Clone()
	cached.Header.Set("X-Agent-Nonce", nonce)
	cached.Header.Set("X-Agent-Signature", ComputeSignature("agent-secret-0123456789", []byte(canonical), "hex"))
	cachedResp, err := client.Do(cached)
	if err != nil {
		t.Fatal(err)
	}
	cachedBody, _ := io.ReadAll(cachedResp.Body)
	cachedResp.Body.Close()
	if cachedResp.StatusCode != 200 || cachedResp.Header.Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("cached replay status=%d headers=%v", cachedResp.StatusCode, cachedResp.Header)
	}
	if !bytes.Equal(firstBody, cachedBody) || cachedResp.Header.Get("X-Agent-Slug") != slug {
		t.Fatal("cached response did not preserve first result metadata")
	}
}
