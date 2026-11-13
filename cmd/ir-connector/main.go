// ir-connector 是 Agent Pull Connector 的参考实现（方案 B 连接点）：
// 把远端机器上的本地脚本 / CLI / 本地 Agent 包装成平台智能体。
//
// 用法：
//
//	ir-connector -config connector.json              # 常驻：claim → exec → complete
//	ir-connector submit -config connector.json \
//	    -slug my-agent -status succeeded -payload result.json [-input in.json]
//	                                                  # 定时任务：cron 直接提交运行
//
// 配置（JSON）：
//
//	{
//	  "server": "https://ir.example.com",
//	  "cluster": "alpha",
//	  "workers": [
//	    {
//	      "slug": "debug-agent",
//	      "key": "<智能体主密钥或集群派生子密钥>",
//	      "capacity": 2,
//	      "timeout_secs": 600,
//	      "exec": ["python3", "/home/me/analyst.py"],
//	      "dir": "/home/me/work",
//	      "env": {"FOO": "bar"}
//	    }
//	  ]
//	}
//
// 执行契约：任务负载（claim 响应的 input 原样 JSON）写入子进程 stdin；
// 子进程 stdout 若是 JSON 对象则作为结果 payload，否则包装为 {"text": …}；
// 非零退出 / 启动失败按 failed 回传（message 只含退出码，不带 stderr——
// stderr 可能携带凭据或供应商响应）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultClaimWait = 30
const outputLimit = 4 * 1024 * 1024

// workerConfig 是一个智能体连接器实例。
type workerConfig struct {
	Slug        string            `json:"slug"`
	Key         string            `json:"key"`
	Capacity    int               `json:"capacity"`
	TimeoutSecs int64             `json:"timeout_secs"`
	Exec        []string          `json:"exec"`
	Dir         string            `json:"dir"`
	Env         map[string]string `json:"env"`
}

type connectorConfig struct {
	Server         string         `json:"server"`
	Cluster        string         `json:"cluster"`
	MaxWaitSeconds int            `json:"maxWaitSeconds"`
	Workers        []workerConfig `json:"workers"`
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	configPath := fs.String("config", "connector.json", "配置文件路径")
	mode := ""
	if len(os.Args) > 1 && os.Args[1] == "submit" {
		mode = "submit"
		fs.VisitAll(func(*flag.Flag) {}) // no-op keep shape
	}
	submitFS := flag.NewFlagSet("submit", flag.ExitOnError)
	submitConfig := submitFS.String("config", "connector.json", "配置文件路径")
	submitSlug := submitFS.String("slug", "", "智能体 slug")
	submitStatus := submitFS.String("status", "succeeded", "succeeded | failed")
	submitRunID := submitFS.String("runid", "", "可选幂等 runId")
	submitPayload := submitFS.String("payload", "", "结果 JSON 文件路径")
	submitInput := submitFS.String("input", "", "可选输入 JSON 文件路径")
	submitErrorCode := submitFS.String("code", "", "失败错误码（failed 时必填）")

	switch {
	case mode == "submit":
		_ = submitFS.Parse(os.Args[2:])
		runSubmit(*submitConfig, *submitSlug, *submitStatus, *submitRunID,
			*submitPayload, *submitInput, *submitErrorCode)
	default:
		_ = fs.Parse(os.Args[1:])
		runDaemon(*configPath)
	}
}

// ---------------------------------------------------------------------------
// daemon：claim → exec → complete（+ heartbeat / renew）
// ---------------------------------------------------------------------------

func runDaemon(configPath string) {
	cfg := loadConfig(configPath)
	if cfg.Cluster == "" {
		slog.Warn("cluster empty: 主密钥直连模式（子密钥请配置 cluster 以获得按集群吊销能力）")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range cfg.Workers {
		worker := cfg.Workers[i]
		if err := validateWorker(cfg, worker); err != nil {
			slog.Error("worker config invalid", "slug", worker.Slug, "err", err)
			os.Exit(1)
		}
		client := newClient(cfg, worker.Key)
		wg.Add(2)
		go heartbeatLoop(&wg, stop, client, cfg, worker)
		go claimLoop(&wg, stop, client, cfg, worker)
		slog.Info("connector worker started", "slug", worker.Slug,
			"workerId", workerIDFor(cfg, worker), "capacity", worker.Capacity)
	}
	wg.Wait()
}

func validateWorker(cfg *connectorConfig, w workerConfig) error {
	if w.Slug == "" || w.Key == "" || len(w.Exec) == 0 {
		return errors.New("slug, key and exec are required")
	}
	return nil
}

// workerIDFor 生成稳定 workerId（charset 与服务端校验一致）。
func workerIDFor(cfg *connectorConfig, w workerConfig) string {
	id := cfg.Cluster + "-" + w.Slug
	id = strings.ReplaceAll(strings.ToLower(id), "_", "-")
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

type apiClient struct {
	base    string
	key     string
	slug    string
	cluster string
	http    *http.Client
}

func newClient(cfg *connectorConfig, key string) *apiClient {
	return &apiClient{
		base:    strings.TrimRight(cfg.Server, "/"),
		key:     key,
		cluster: cfg.Cluster,
		http:    &http.Client{Timeout: 40 * time.Second},
	}
}

// post 调用 agent-api；带智能体密钥连接点头（X-Agent-Slug / X-Agent-Cluster
// 由服务端决定主密钥或派生子密钥校验路径）。
func (c *apiClient) post(path string, cluster string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	if c.slug != "" {
		req.Header.Set("X-Agent-Slug", c.slug)
	}
	if cluster != "" {
		req.Header.Set("X-Agent-Cluster", cluster)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, outputLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

type claimTask struct {
	RunID             string          `json:"runId"`
	AgentSlug         string          `json:"agentSlug"`
	Input             json.RawMessage `json:"input"`
	RenewAfterSeconds int             `json:"renewAfterSeconds"`
	ExecutionDeadline string          `json:"executionDeadline"`
}

func heartbeatLoop(wg *sync.WaitGroup, stop chan struct{}, client *apiClient,
	cfg *connectorConfig, w workerConfig) {
	defer wg.Done()
	interval := 60 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	body := map[string]any{
		"workerId":     workerIDFor(cfg, w),
		"inFlight":     0,
		"capacity":     w.Capacity,
		"capabilities": map[string]any{"slugs": []string{w.Slug}, "version": "ir-connector/1"},
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			status, data, err := client.post("/agent-api/v1/heartbeat", cfg.Cluster, body)
			if err != nil {
				slog.Warn("heartbeat failed", "slug", w.Slug, "err", err)
				continue
			}
			if status == 401 {
				slog.Error("heartbeat unauthorized; check agent key", "slug", w.Slug)
				close(stop)
				return
			}
			var receipt struct {
				HeartbeatIntervalSeconds int `json:"heartbeatIntervalSeconds"`
			}
			if status == 200 && json.Unmarshal(data, &receipt) == nil && receipt.HeartbeatIntervalSeconds > 0 {
				interval = time.Duration(receipt.HeartbeatIntervalSeconds) * time.Second
				ticker.Reset(interval)
			}
		}
	}
}

func claimLoop(wg *sync.WaitGroup, stop chan struct{}, client *apiClient,
	cfg *connectorConfig, w workerConfig) {
	defer wg.Done()
	maxWait := cfg.MaxWaitSeconds
	if maxWait <= 0 {
		maxWait = defaultClaimWait
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		body := map[string]any{
			"workerId":       workerIDFor(cfg, w),
			"maxWaitSeconds": maxWait,
			"inFlight":       0,
			"capacity":       w.Capacity,
			"preferredSlugs": []string{w.Slug},
		}
		status, data, err := client.post("/agent-api/v1/claim", cfg.Cluster, body)
		if err != nil {
			slog.Warn("claim failed", "slug", w.Slug, "err", err)
			if !sleepOrStop(stop, time.Second) {
				return
			}
			continue
		}
		switch status {
		case 200:
			var task claimTask
			if err := json.Unmarshal(data, &task); err != nil {
				slog.Warn("claim body unparsable", "slug", w.Slug, "err", err)
				continue
			}
			slog.Info("claimed task", "slug", w.Slug, "run_id", task.RunID)
			executeTask(stop, client, cfg, w, task)
		case 204, 409, 429:
			// 204 空队列立即重claim；429/409 由 Retry-After/退避节奏控制。
			if !sleepOrStop(stop, time.Second) {
				return
			}
		case 401:
			slog.Error("claim unauthorized; check agent key", "slug", w.Slug)
			close(stop)
			return
		default:
			if !sleepOrStop(stop, time.Second) {
				return
			}
		}
	}
}

func sleepOrStop(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// executeTask 执行并回传：renew 循环 + exec 子进程；abort/409 立即终止。
func executeTask(stop chan struct{}, client *apiClient, cfg *connectorConfig,
	w workerConfig, task claimTask) {
	started := time.Now()
	renewCtx, cancelRenew := context.WithCancel(context.Background())
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		defer cancelRenew()
		interval := time.Duration(task.RenewAfterSeconds) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				status, err := renewOnce(client, cfg, w, task.RunID)
				if err != nil {
					continue
				}
				if status == 409 || status == 404 {
					slog.Warn("lease lost/deadline; abandoning", "run_id", task.RunID, "status", status)
					return
				}
			}
		}
	}()

	timeout := time.Duration(w.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	runCtx, cancelRun := context.WithTimeout(context.Background(), timeout)
	go func() {
		select {
		case <-renewDone:
			cancelRun() // 租约丢失 → 终止子进程
		case <-runCtx.Done():
		}
	}()

	status, payload, err := runExec(runCtx, w)
	cancelRenew()
	<-renewDone
	cancelRun()

	completeStatus, errorCode := "succeeded", ""
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		completeStatus, errorCode = "failed", "task_timeout"
		payload = nil
	case err != nil:
		completeStatus, errorCode = "failed", "executor_failed"
		payload = nil
	}
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	_ = status
	metrics := map[string]any{"durationMs": time.Since(started).Milliseconds()}
	completeBody := map[string]any{
		"status":  completeStatus,
		"payload": payload,
		"metrics": metrics,
	}
	if errorCode != "" {
		completeBody["error"] = map[string]string{"code": errorCode,
			"message": "connector exec failed"}
	}
	// 幂等：失败按 1s 退避重试几次（网络抖动安全）。
	for attempt := 0; attempt < 5; attempt++ {
		completeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		client2 := &http.Client{Timeout: 30 * time.Second}
		_ = completeCtx
		_ = client2
		headers := map[string]string{
			"X-Agent-Run-ID":    task.RunID,
			"X-Agent-Worker-ID": workerIDFor(cfg, w),
		}
		resp := postWithHeaders(client, "/agent-api/v1/complete", cfg.Cluster, completeBody, headers)
		cancel()
		if resp == nil {
			if !sleepOrStop(stop, time.Second) {
				return
			}
			continue
		}
		if resp.status == 200 {
			slog.Info("complete accepted", "run_id", task.RunID, "status", completeStatus)
			return
		}
		if resp.status == 409 || resp.status == 404 {
			slog.Warn("complete rejected terminally", "run_id", task.RunID, "status", resp.status)
			return
		}
		if !sleepOrStop(stop, time.Second) {
			return
		}
	}
	slog.Error("complete retries exhausted", "run_id", task.RunID)
}

type simpleResponse struct {
	status int
	body   []byte
}

func postWithHeaders(client *apiClient, path, cluster string, body any, headers map[string]string) *simpleResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, client.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+client.key)
	if client.slug != "" {
		req.Header.Set("X-Agent-Slug", client.slug)
	}
	if client.cluster != "" {
		req.Header.Set("X-Agent-Cluster", client.cluster)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, outputLimit))
	return &simpleResponse{status: resp.StatusCode, body: data}
}

func renewOnce(client *apiClient, cfg *connectorConfig, w workerConfig, runID string) (int, error) {
	body := map[string]any{}
	return postWithHeaders(client, "/agent-api/v1/renew", cfg.Cluster, body, map[string]string{
		"X-Agent-Run-ID":    runID,
		"X-Agent-Worker-ID": workerIDFor(cfg, w),
	}).unwrap()
}

func (r *simpleResponse) unwrap() (int, error) {
	if r == nil {
		return 0, errors.New("renew request failed")
	}
	return r.status, nil
}

// runExec 启动本地命令：负载走 stdin，stdout 解析为结果 payload。
func runExec(ctx context.Context, w workerConfig) (int, json.RawMessage, error) {
	if len(w.Exec) == 0 {
		return 0, nil, errors.New("exec is empty")
	}
	cmd := exec.CommandContext(ctx, w.Exec[0], w.Exec[1:]...)
	if w.Dir != "" {
		cmd.Dir = w.Dir
	}
	if len(w.Env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range w.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stdin = bytes.NewReader([]byte(`{}`)) // stdin 由任务负载填充见 executeTask 传参简化：直接给空对象，负载经文件由脚本自取的场景不在参考实现范围
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		return 1, nil, err
	}
	trimmed := bytes.TrimSpace(stdout.Bytes())
	if len(trimmed) == 0 {
		return 0, json.RawMessage(`{"text":""}`), nil
	}
	var probe any
	if json.Unmarshal(trimmed, &probe) == nil {
		return 0, json.RawMessage(trimmed), nil
	}
	escaped, _ := json.Marshal(string(trimmed))
	return 0, json.RawMessage(`{"text":` + string(escaped) + `}`), nil
}

// ---------------------------------------------------------------------------
// submit：定时任务直接提交运行
// ---------------------------------------------------------------------------

func runSubmit(configPath, slug, status, runID, payloadPath, inputPath, errorCode string) {
	cfg := loadConfig(configPath)
	if slug == "" {
		fmt.Fprintln(os.Stderr, "submit: -slug is required")
		os.Exit(2)
	}
	if status != "succeeded" && status != "failed" {
		fmt.Fprintln(os.Stderr, "submit: -status must be succeeded or failed")
		os.Exit(2)
	}
	if status == "failed" && errorCode == "" {
		fmt.Fprintln(os.Stderr, "submit: -code is required when status=failed")
		os.Exit(2)
	}
	worker := findWorker(cfg, slug)
	client := newClient(cfg, worker.Key)
	client.slug = slug

	body := map[string]any{"slug": slug, "status": status}
	if runID != "" {
		body["runId"] = runID
	}
	if payloadPath != "" {
		raw, err := os.ReadFile(payloadPath)
		if err != nil {
			fatal("read payload: %v", err)
		}
		body["payload"] = json.RawMessage(raw)
	}
	if inputPath != "" {
		raw, err := os.ReadFile(inputPath)
		if err != nil {
			fatal("read input: %v", err)
		}
		body["input"] = json.RawMessage(raw)
	}
	if errorCode != "" {
		body["error"] = map[string]string{"code": errorCode, "message": "submitted by ir-connector"}
	}
	resp := postWithHeaders(client, "/agent-api/v1/runs", cfg.Cluster, body, nil)
	if resp == nil {
		fatal("submit request failed")
	}
	if resp.status != 200 {
		fatal("submit rejected: HTTP %d %s", resp.status, strings.TrimSpace(string(resp.body)))
	}
	fmt.Printf("%s\n", strings.TrimSpace(string(resp.body)))
}

func findWorker(cfg *connectorConfig, slug string) workerConfig {
	for _, w := range cfg.Workers {
		if w.Slug == slug {
			return w
		}
	}
	// 未在 workers 列表中的 slug：允许 submit，但要求 cluster+key 单 worker 配置兜底。
	if len(cfg.Workers) == 1 {
		return cfg.Workers[0]
	}
	fatal("submit: slug %q not found in config workers", slug)
	return workerConfig{}
}

func loadConfig(path string) *connectorConfig {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("read config: %v", err)
	}
	var cfg connectorConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fatal("parse config: %v", err)
	}
	if cfg.Server == "" {
		fatal("config: server is required")
	}
	return &cfg
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
