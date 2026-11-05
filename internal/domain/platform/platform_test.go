package platform

import (
	"encoding/json"
	"testing"
)

func TestSignAndVerifyBodySignature(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-value-32-bytes-long!")
	body := []byte(`{"schemaVersion":"1.0","runId":"run-1"}`)

	hexSig := EncodeSignature(SignBody(secret, body), EncodingHex)
	if !VerifyBodySignature(secret, body, hexSig, EncodingHex) {
		t.Error("hex signature must verify")
	}
	b64Sig := EncodeSignature(SignBody(secret, body), EncodingBase64)
	if !VerifyBodySignature(secret, body, b64Sig, EncodingBase64) {
		t.Error("base64 signature must verify")
	}
	if VerifyBodySignature(secret, []byte("tampered"), hexSig, EncodingHex) {
		t.Error("tampered body must not verify")
	}
	if VerifyBodySignature([]byte("other-secret-32-bytes-long!!!!!!"), body, hexSig, EncodingHex) {
		t.Error("wrong secret must not verify")
	}
}

func TestParseEncoding(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"hex", "HEX", " base64 "} {
		if _, err := ParseEncoding(in); err != nil {
			t.Errorf("ParseEncoding(%q): %v", in, err)
		}
	}
	if _, err := ParseEncoding("md5"); err == nil {
		t.Error("unknown encoding must be rejected")
	}
}

func TestClassifyReceipts(t *testing.T) {
	t.Parallel()
	okBody, _ := json.Marshal(map[string]any{"code": "OK", "accepted": true, "runStatus": "completed"})
	if r := Classify(200, okBody); r.Classification != ReceiptAccepted {
		t.Errorf("200+OK+accepted = %q, want accepted", r.Classification)
	}
	rejectBody, _ := json.Marshal(map[string]any{"code": "REJECTED", "accepted": false})
	if r := Classify(200, rejectBody); r.Classification != ReceiptRejected {
		t.Errorf("200+REJECTED = %q, want rejected", r.Classification)
	}
	if r := Classify(200, []byte("not json")); r.Classification != ReceiptUncertain {
		t.Errorf("200+garbage = %q, want uncertain", r.Classification)
	}
	for _, status := range []int{408, 429, 500, 502, 503} {
		if r := Classify(status, nil); r.Classification != ReceiptDeliveryFailed {
			t.Errorf("status %d = %q, want delivery_failed", status, r.Classification)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 409} {
		if r := Classify(status, nil); r.Classification != ReceiptUncertain {
			t.Errorf("status %d = %q, want uncertain", status, r.Classification)
		}
	}
}

func TestBoundTaskEnvelopeParses(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"schemaVersion":"1.0","runId":"run-1","agentCode":"demo","positionType":"FUNDAMENTAL_ANALYST","templateInputData":[],"taskBrief":{},"callbackUrl":"https://platform.example.com/callback"}`)
	var task BoundTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if task.RunID == nil || *task.RunID != "run-1" ||
		task.AgentCode == nil || *task.AgentCode != "demo" ||
		task.PositionType == nil || *task.PositionType != "FUNDAMENTAL_ANALYST" {
		t.Errorf("envelope = %+v", task)
	}
}
