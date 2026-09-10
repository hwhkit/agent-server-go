package mota

// Push 入口的字段级校验测试。对应 rs src/mota/push.rs 的 validate_task 与请求头/
// 元数据校验部分。信封构造复用 push_test.go 里的 pushTaskBody / pushTaskBodyWith,
// 服务器构造复用 newPushServer / postPush。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// pushValidationErrorFields 从 422 响应体解出 fields[].field -> message 映射。
func pushValidationErrorFields(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Fields []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		t.Fatalf("bad problem+json body: %v\nraw: %s", err, raw)
	}
	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected status 422 in body, got %d", problem.Status)
	}
	if problem.Title != "Validation Failed" {
		t.Fatalf("expected title %q, got %q", "Validation Failed", problem.Title)
	}
	out := make(map[string]string, len(problem.Fields))
	for _, f := range problem.Fields {
		out[f.Field] = f.Message
	}
	return out
}

// requirePush422WithField 发一次带自定义信封覆盖的请求,断言 422 且 fields 里
// 出现指定字段。
func requirePush422WithField(
	t *testing.T, srv *httptest.Server, runID string, overrides map[string]any, wantField string,
) {
	t.Helper()
	body := []byte(pushTaskBodyWith(runID, overrides))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.StatusCode, raw)
	}
	fields := pushValidationErrorFields(t, raw)
	if _, ok := fields[wantField]; !ok {
		t.Fatalf("expected a validation error on field %q, got fields %v (raw: %s)",
			wantField, fields, raw)
	}
}

// requirePushOKWithOverrides 发一次带覆盖的请求,断言 200(用于确认某个变体本身合法)。
func requirePushOKWithOverrides(
	t *testing.T, srv *httptest.Server, runID string, overrides map[string]any,
) []byte {
	t.Helper()
	body := []byte(pushTaskBodyWith(runID, overrides))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	return raw
}

// ---------------------------------------------------------------------------
// schemaVersion / runId / agentCode / callbackUrl 必填与精确匹配
// ---------------------------------------------------------------------------

func TestPushValidationSchemaVersionMustEqual10(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"schemaVersion": "2.0"}, "schemaVersion")
}

func TestPushValidationSchemaVersionMissing(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"schemaVersion": nil}, "schemaVersion")
}

func TestPushValidationAgentCodeRequired(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"agentCode": ""}, "agentCode")
}

func TestPushValidationCallbackURLRequired(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"callbackUrl": nil}, "callbackUrl")
}

// runId 缺失时同时缺 required 与 mismatch 两条错误,但只要求 runId 一定出现。
func TestPushValidationRunIDMustMatchHeader(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	headerRunID := unique("header-run")
	bodyRunID := unique("body-run")
	requirePush422WithField(t, srv, headerRunID,
		map[string]any{"runId": bodyRunID}, "runId")
}

func TestPushValidationRunIDMissing(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"runId": nil}, "runId")
}

// ---------------------------------------------------------------------------
// positionType: 8 个受支持的岗位类型逐个放行,未知值拒绝
// ---------------------------------------------------------------------------

func TestPushValidationAllEightPositionTypesAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	for position := range pushPositionTypes {
		t.Run(position, func(t *testing.T) {
			runID := unique("run")
			requirePushOKWithOverrides(t, srv, runID, map[string]any{"positionType": position})
		})
	}
}

func TestPushValidationUnknownPositionTypeRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"positionType": "UNKNOWN_ROLE"}, "positionType")
}

// positionType 是条件字段:整体缺失(如探测任务)必须放行。
func TestPushValidationMissingPositionTypeIsAllowed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"positionType": nil})
}

// ---------------------------------------------------------------------------
// templateInputData: 必需、名字不能空、不能重名
// ---------------------------------------------------------------------------

func TestPushValidationTemplateInputDataRequired(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"templateInputData": nil}, "templateInputData")
}

func TestPushValidationTemplateInputDataEmptyNameRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"templateInputData": []any{
			map[string]any{"name": "  ", "value": "x"},
		},
	}, "templateInputData[0].name")
}

func TestPushValidationTemplateInputDataDuplicateNameRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"templateInputData": []any{
			map[string]any{"name": "symbols", "value": []any{"AAPL"}},
			map[string]any{"name": "symbols", "value": []any{"MSFT"}},
		},
	}, "templateInputData[1].name")
}

// 空数组本身是合法的:没有入参可评估,不算错误。
func TestPushValidationTemplateInputDataEmptyArrayAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"templateInputData": []any{}})
}

// ---------------------------------------------------------------------------
// taskBrief: 必须是对象
// ---------------------------------------------------------------------------

func TestPushValidationTaskBriefMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"taskBrief": "not-an-object"}, "taskBrief")
}

func TestPushValidationTaskBriefArrayRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"taskBrief": []any{"x"}}, "taskBrief")
}

// taskBrief 缺失是允许的(探测任务不带)。
func TestPushValidationTaskBriefMissingIsAllowed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"taskBrief": nil})
}

// ---------------------------------------------------------------------------
// globalParameters: 键白名单 + 各字段类型
// ---------------------------------------------------------------------------

func TestPushValidationGlobalParametersMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"globalParameters": "not-an-object"}, "globalParameters")
}

func TestPushValidationGlobalParametersRejectsUnknownKey(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"unknownKey": "x"},
	}, "globalParameters.unknownKey")
}

func TestPushValidationGlobalParametersAllFourKeysAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{
			"language":      "en-US",
			"targetMarkets": []any{"US", "HK"},
			"symbolPool":    []any{"AAPL", "MSFT"},
			"universeMode":  "SEED",
		},
	})
}

func TestPushValidationGlobalParametersLanguageMustBeString(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"language": 123},
	}, "globalParameters.language")
}

func TestPushValidationGlobalParametersTargetMarketsMustBeStringArray(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"targetMarkets": []any{1, 2}},
	}, "globalParameters.targetMarkets")
}

func TestPushValidationSymbolPoolRejectsBlankEntries(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"symbolPool": []any{"AAPL", "  "}},
	}, "globalParameters.symbolPool")
}

// targetMarkets 允许空白字符串,只有 symbolPool 不允许 —— 二者规则不同,必须分别测。
func TestPushValidationTargetMarketsAllowsBlankEntries(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"targetMarkets": []any{"US", ""}},
	})
}

func TestPushValidationUniverseModeMustBeRestrictedOrSeed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"universeMode": "BOGUS"},
	}, "globalParameters.universeMode")
}

func TestPushValidationUniverseModeRestrictedAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"globalParameters": map[string]any{"universeMode": "RESTRICTED"},
	})
}

// globalParameters 整体缺失是允许的。
func TestPushValidationGlobalParametersMissingIsAllowed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"globalParameters": nil})
}

// ---------------------------------------------------------------------------
// upstream: DAG 结构(self / nodes / edges)
// ---------------------------------------------------------------------------

func TestPushValidationUpstreamMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{"upstream": "nope"}, "upstream")
}

func TestPushValidationUpstreamSelfMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstream": map[string]any{"self": "nope", "nodes": []any{}, "edges": []any{}},
	}, "upstream.self")
}

func TestPushValidationUpstreamNodesMustBeArray(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstream": map[string]any{"self": map[string]any{}, "nodes": "nope", "edges": []any{}},
	}, "upstream.nodes")
}

func TestPushValidationUpstreamEdgesMustBeArray(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstream": map[string]any{"self": map[string]any{}, "nodes": []any{}, "edges": "nope"},
	}, "upstream.edges")
}

func TestPushValidationUpstreamNodeMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstream": map[string]any{
			"self": map[string]any{}, "nodes": []any{"not-an-object"}, "edges": []any{},
		},
	}, "upstream.nodes[0]")
}

func TestPushValidationUpstreamEdgeRequiresNonEmptyFromAndTo(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstream": map[string]any{
			"self":  map[string]any{},
			"nodes": []any{},
			"edges": []any{map[string]any{"from": "", "to": "b"}},
		},
	}, "upstream.edges[0]")
}

func TestPushValidationUpstreamWellFormedAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"upstream": map[string]any{
			"self":  map[string]any{"agentInstanceId": "self-1"},
			"nodes": []any{map[string]any{"agentInstanceId": "n1"}},
			"edges": []any{map[string]any{"from": "n1", "to": "self-1"}},
		},
	})
}

// upstream 整体缺失是允许的。
func TestPushValidationUpstreamMissingIsAllowed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"upstream": nil})
}

// ---------------------------------------------------------------------------
// upstreamPayload: 每项都要有非空 agentInstanceId 与存在的 payloadResult
// ---------------------------------------------------------------------------

func TestPushValidationUpstreamPayloadMustBeArray(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"upstreamPayload": "nope"}, "upstreamPayload")
}

func TestPushValidationUpstreamPayloadItemMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstreamPayload": []any{"not-an-object"},
	}, "upstreamPayload[0]")
}

func TestPushValidationUpstreamPayloadAgentInstanceIDRequired(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstreamPayload": []any{
			map[string]any{"agentInstanceId": "  ", "payloadResult": map[string]any{}},
		},
	}, "upstreamPayload[0].agentInstanceId")
}

// payloadResult 只需要存在,值本身允许是 null。
func TestPushValidationUpstreamPayloadResultMustExistButMayBeNull(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"upstreamPayload": []any{
			map[string]any{"agentInstanceId": "a1"},
		},
	}, "upstreamPayload[0].payloadResult")

	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"upstreamPayload": []any{
			map[string]any{"agentInstanceId": "a1", "payloadResult": nil},
		},
	})
}

// ---------------------------------------------------------------------------
// runtimeContext: 只允许 isFinal,且必须是布尔
// ---------------------------------------------------------------------------

func TestPushValidationRuntimeContextMustBeObject(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"runtimeContext": "nope"}, "runtimeContext")
}

func TestPushValidationRuntimeContextRejectsUnknownKey(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"runtimeContext": map[string]any{"isFinal": true, "extra": "x"},
	}, "runtimeContext.extra")
}

func TestPushValidationRuntimeContextIsFinalMustBeBoolean(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID, map[string]any{
		"runtimeContext": map[string]any{"isFinal": "true"},
	}, "runtimeContext.isFinal")
}

func TestPushValidationRuntimeContextAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{
		"runtimeContext": map[string]any{"isFinal": false},
	})
}

// runtimeContext 整体缺失是允许的。
func TestPushValidationRuntimeContextMissingIsAllowed(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"runtimeContext": nil})
}

// ---------------------------------------------------------------------------
// callbackUrl: 绝对 HTTPS,HTTP 只对回环放行
// ---------------------------------------------------------------------------

func TestPushValidationCallbackURLHTTPSAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePushOKWithOverrides(t, srv, runID, map[string]any{"callbackUrl": "https://cb.example/cb"})
}

func TestPushValidationCallbackURLPlainHTTPRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"callbackUrl": "http://cb.example/cb"}, "callbackUrl")
}

func TestPushValidationCallbackURLLoopbackHTTPAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		requirePushOKWithOverrides(t, srv, runID, map[string]any{
			"callbackUrl": fmt.Sprintf("http://%s:9999/cb", host),
		})
		runID = unique("run")
	}
}

func TestPushValidationCallbackURLMalformedRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"callbackUrl": "not a url"}, "callbackUrl")
}

func TestPushValidationCallbackURLRelativeRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	requirePush422WithField(t, srv, runID,
		map[string]any{"callbackUrl": "/relative/path"}, "callbackUrl")
}

// ---------------------------------------------------------------------------
// Content-Type: 只接受 application/json(允许带参数)
// ---------------------------------------------------------------------------

func TestPushRejectsUnsupportedContentType(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(RunIDHeader, runID)
	req.Header.Set(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", resp.StatusCode)
	}
}

func TestPushAcceptsJSONContentTypeWithCharsetParameter(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(RunIDHeader, runID)
	req.Header.Set(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 请求头重复 → 422
// ---------------------------------------------------------------------------

func TestPushRejectsDuplicateRunIDHeader(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, sig)
	req.Header.Add(RunIDHeader, runID)
	req.Header.Add(RunIDHeader, runID)
	req.Header.Set(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
}

func TestPushRejectsDuplicateIdempotencyKeyHeader(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(RunIDHeader, runID)
	req.Header.Add(IdempotencyKeyHeader, unique("idem"))
	req.Header.Add(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
}

func TestPushRejectsDuplicateSignatureHeader(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add(SignatureHeader, sig)
	req.Header.Add(SignatureHeader, sig)
	req.Header.Set(RunIDHeader, runID)
	req.Header.Set(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
}

// 缺失签名头是凭证问题 → 401,不是 422(与「重复」区分开)。
func TestPushMissingSignatureHeaderIs401NotValidation(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mota/push", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(RunIDHeader, runID)
	req.Header.Set(IdempotencyKeyHeader, unique("idem"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// runId / 幂等键长度上限:200 / 240
// ---------------------------------------------------------------------------

func TestPushRunIDAtMaxLengthAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := strings.Repeat("a", 200)
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestPushRunIDOverMaxLengthRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := strings.Repeat("a", 201)
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.StatusCode, raw)
	}
	fields := pushValidationErrorFields(t, raw)
	if _, ok := fields["X-MoTA-Run-ID"]; !ok {
		t.Fatalf("expected a validation error on X-MoTA-Run-ID, got %v", fields)
	}
}

func TestPushIdempotencyKeyAtMaxLengthAccepted(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	key := strings.Repeat("k", 240)
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

func TestPushIdempotencyKeyOverMaxLengthRejected(t *testing.T) {
	srv := newPushServer(t, testConfig(), PlaceholderProcessor{})
	runID := unique("run")
	key := strings.Repeat("k", 241)
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, key)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.StatusCode, raw)
	}
	fields := pushValidationErrorFields(t, raw)
	if _, ok := fields["X-MoTA-Idempotency-Key"]; !ok {
		t.Fatalf("expected a validation error on X-MoTA-Idempotency-Key, got %v", fields)
	}
}

// validatePushMetadata 的空白/控制字符校验直接测函数,而不经 HTTP:HTTP 头解析
// 本身会按 RFC 7230 剥掉首尾 OWS(空格/tab),经传输层发一个带前导空格的头
// 根本测不到这条校验 —— 到服务端时空格已经被去掉了。
func TestValidatePushMetadataRejectsOuterWhitespace(t *testing.T) {
	if apiErr := validatePushMetadata(" run-with-space", "X-MoTA-Run-ID", 200); apiErr == nil {
		t.Fatal("expected a validation error for outer whitespace")
	}
}

func TestValidatePushMetadataRejectsControlCharacters(t *testing.T) {
	if apiErr := validatePushMetadata("run\x01withcontrol", "X-MoTA-Run-ID", 200); apiErr == nil {
		t.Fatal("expected a validation error for an embedded control character")
	}
}

func TestValidatePushMetadataRejectsEmpty(t *testing.T) {
	if apiErr := validatePushMetadata("", "X-MoTA-Run-ID", 200); apiErr == nil {
		t.Fatal("expected a validation error for an empty value")
	}
}

func TestValidatePushMetadataAcceptsCleanValue(t *testing.T) {
	if apiErr := validatePushMetadata("clean-run-id", "X-MoTA-Run-ID", 200); apiErr != nil {
		t.Fatalf("expected no error, got %v", apiErr)
	}
}

// ---------------------------------------------------------------------------
// 504:同步执行超过安全预算就放弃等待
// ---------------------------------------------------------------------------

// sleepyProcessor 睡得比配置的超时长,用来触发 504/超时回调路径。
type sleepyProcessor struct{ delay time.Duration }

func (p sleepyProcessor) Process(ctx context.Context, task *TaskContext) (*ProcessedOutput, error) {
	select {
	case <-time.After(p.delay):
		return PlaceholderOutput("too late"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestPushSyncTimeoutReturns504(t *testing.T) {
	cfg := testConfig()
	cfg.TaskTimeout = 200 * time.Millisecond
	srv := newPushServer(t, cfg, sleepyProcessor{delay: 2 * time.Second})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "urn:mota:task-timeout") {
		t.Fatalf("expected urn:mota:task-timeout, got %s", raw)
	}
}

// 超时后的租约必须被释放,否则平台重投只会拿到 409(而不是重新执行)。
//
// NewPushHandler 每次都新建内存幂等存储,所以要验证「同一个 handler、同一个键、
// 先超时再重试」,必须自己拼 PushHandler 而不是用 newPushServer 的两遍调用。
func TestPushSyncTimeoutReleasesLeaseForRetry(t *testing.T) {
	cfg := testConfig()
	cfg.TaskTimeout = 200 * time.Millisecond

	handler := &PushHandler{
		Config:      cfg,
		Idempotency: NewInMemoryPushIdempotencyStore(),
		Execution:   NewProcessorBackend(sleepyProcessor{delay: 2 * time.Second}),
	}
	r := chi.NewRouter()
	r.Post(cfg.PushPath, handler.ServeHTTP)
	srv := httptest.NewServer(r)
	defer srv.Close()

	runID := unique("run")
	key := unique("idem")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")

	resp1, raw1 := postPush(t, srv.URL, body, sig, runID, key)
	if resp1.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 on first attempt, got %d: %s", resp1.StatusCode, raw1)
	}

	// 换一个不会超时的执行后端,同一个幂等存储必须已经放开了租约。
	handler.Execution = NewProcessorBackend(PlaceholderProcessor{})
	resp2, raw2 := postPush(t, srv.URL, body, sig, runID, key)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on retry after timeout, got %d: %s", resp2.StatusCode, raw2)
	}
}

// ---------------------------------------------------------------------------
// 502:受信通道拿不到 FINAL usage 就必须失败,不能静默降级
// ---------------------------------------------------------------------------

func TestPushTrustedUsageUnavailableReturns502(t *testing.T) {
	cfg := testConfig()
	cfg.TrustedUsageReportingEnabled = true
	// PlaceholderProcessor 从不产出 Usage,天然命中 usage-unavailable 路径。
	srv := newPushServer(t, cfg, PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "urn:mota:usage-unavailable") {
		t.Fatalf("expected urn:mota:usage-unavailable, got %s", raw)
	}
}

// 未开启受信上报时,缺 usage 完全不影响正常返回 200。
func TestPushUntrustedMissingUsageStillReturns200(t *testing.T) {
	cfg := testConfig()
	cfg.TrustedUsageReportingEnabled = false
	srv := newPushServer(t, cfg, PlaceholderProcessor{})
	runID := unique("run")
	body := []byte(pushTaskBody(runID))
	sig := ComputeSignature(testSecret, body, "hex")
	resp, raw := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
}

// ---------------------------------------------------------------------------
// 异步保守载荷:6 个岗位形状 + 默认(FUNDAMENTAL_ANALYST 等映射到 assessments)
// ---------------------------------------------------------------------------

func TestConservativeFailurePayloadShapesByPositionType(t *testing.T) {
	english := strPtr("en-US")
	cases := []struct {
		position string
		checkKey string
	}{
		{"TECHNICAL_ANALYST", "signals"},
		{"PORTFOLIO_MANAGER", "decision_type"},
		{"RISK_MANAGER", "gate"},
		{"EXECUTION_TRADER", "orders"},
		{"STRATEGIST", "signals"},
		// 未列出的岗位(含 FUNDAMENTAL_ANALYST/QUANT_TRADING_AGENT/SENTINEL/
		// 未知值)统一落到默认的 assessments 骨架。
		{"FUNDAMENTAL_ANALYST", "assessments"},
		{"QUANT_TRADING_AGENT", "assessments"},
		{"", "assessments"},
	}
	for _, tc := range cases {
		t.Run(tc.position, func(t *testing.T) {
			payload := conservativeFailurePayload(tc.position, english)
			if payload.ContractVersion != "V2" {
				t.Fatalf("contract_version = %q, want V2", payload.ContractVersion)
			}
			result, ok := payload.PayloadResult.(map[string]any)
			if !ok {
				t.Fatalf("payload_result is not an object: %v", payload.PayloadResult)
			}
			if _, present := result[tc.checkKey]; !present {
				t.Fatalf("position %q: expected key %q in payload_result, got %v",
					tc.position, tc.checkKey, result)
			}
		})
	}
}

// PORTFOLIO_MANAGER 的保守骨架必须是 NO_ACTION,不能是别的决策类型。
func TestConservativeFailurePayloadPortfolioManagerIsNoAction(t *testing.T) {
	payload := conservativeFailurePayload("PORTFOLIO_MANAGER", nil)
	result := payload.PayloadResult.(map[string]any)
	if result["decision_type"] != "NO_ACTION" {
		t.Fatalf("decision_type = %v, want NO_ACTION", result["decision_type"])
	}
}

// RISK_MANAGER 的保守骨架必须是 WARN(偏保守),且 checks[0].reasoning 硬编码
// 中文,与传入的 language 无关。
func TestConservativeFailurePayloadRiskManagerIsWarnWithHardcodedChineseReasoning(t *testing.T) {
	for _, language := range []*string{nil, strPtr("en-US"), strPtr("zh-CN")} {
		payload := conservativeFailurePayload("RISK_MANAGER", language)
		result := payload.PayloadResult.(map[string]any)
		if result["gate"] != "WARN" {
			t.Fatalf("gate = %v, want WARN", result["gate"])
		}
		checks, ok := result["checks"].([]any)
		if !ok || len(checks) != 1 {
			t.Fatalf("checks = %v, want a single-element array", result["checks"])
		}
		check := checks[0].(map[string]any)
		if check["reasoning"] != "任务处理失败，已返回不采取行动的保守结果。" {
			t.Fatalf("language %v: reasoning = %v, want the hardcoded Chinese text",
				language, check["reasoning"])
		}
	}
}

// payload_info 的语言判定看 language 前缀 zh,大小写不敏感;summary 才会切中文。
func TestConservativeFailurePayloadInfoSwitchesLanguageOnZhPrefix(t *testing.T) {
	zh := conservativeFailurePayload("EXECUTION_TRADER", strPtr("zh-CN"))
	if zh.PayloadInfo != "任务处理失败，已返回不采取行动的保守结果。" {
		t.Fatalf("payload_info = %q, want the Chinese summary", zh.PayloadInfo)
	}
	en := conservativeFailurePayload("EXECUTION_TRADER", strPtr("en-US"))
	if en.PayloadInfo != "Task processing failed; returning a conservative no-action result." {
		t.Fatalf("payload_info = %q, want the English summary", en.PayloadInfo)
	}
	none := conservativeFailurePayload("EXECUTION_TRADER", nil)
	if none.PayloadInfo != "Task processing failed; returning a conservative no-action result." {
		t.Fatalf("payload_info = %q, want the English default when language is nil", none.PayloadInfo)
	}
}

// 端到端:异步执行超时时,非受信通道必须收到某个合乎岗位形状的保守回调。
func TestPushAsyncTimeoutDeliversConservativePayload(t *testing.T) {
	cbSrv, cb := newCallbackServer(http.StatusOK)
	cfg := testConfig()
	cfg.PushMode = PushModeAsync
	cfg.TaskTimeout = 200 * time.Millisecond
	srv := newPushServer(t, cfg, sleepyProcessor{delay: 2 * time.Second})

	runID := unique("run-async-timeout")
	body := []byte(pushTaskBodyWith(runID, map[string]any{
		"callbackUrl":  cbSrv.URL,
		"positionType": "RISK_MANAGER",
	}))
	sig := ComputeSignature(testSecret, body, "hex")

	resp, _ := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	waitForCondition(t, 5*time.Second, "conservative callback", func() bool { return cb.count() > 0 })

	item := cb.first()
	var payload PayloadV1
	if err := json.Unmarshal(item.body, &payload); err != nil {
		t.Fatalf("bad callback body: %v", err)
	}
	result := payload.PayloadResult.(map[string]any)
	if result["gate"] != "WARN" {
		t.Fatalf("gate = %v, want WARN (conservative risk_manager skeleton)", result["gate"])
	}
}

// 受信通道下,异步超时必须抑制回调,而不是发一份没有 usage 的保守载荷。
func TestPushAsyncTrustedTimeoutSuppressesCallback(t *testing.T) {
	cbSrv, cb := newCallbackServer(http.StatusOK)
	cfg := testConfig()
	cfg.PushMode = PushModeAsync
	cfg.TrustedUsageReportingEnabled = true
	cfg.TaskTimeout = 200 * time.Millisecond
	srv := newPushServer(t, cfg, sleepyProcessor{delay: 2 * time.Second})

	runID := unique("run-async-trusted-timeout")
	body := []byte(pushTaskBodyWith(runID, map[string]any{"callbackUrl": cbSrv.URL}))
	sig := ComputeSignature(testSecret, body, "hex")

	resp, _ := postPush(t, srv.URL, body, sig, runID, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	// 给后台协程足够时间执行完(它会在 ~200ms 内放弃等待);之后确认没有回调。
	time.Sleep(1 * time.Second)
	if got := cb.count(); got != 0 {
		t.Fatalf("expected callback to be suppressed for a trusted timed-out run, got %d deliveries", got)
	}
}
