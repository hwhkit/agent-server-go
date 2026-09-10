package mota

import (
	"strings"
	"testing"
)

func TestSignatureHexRoundtrip(t *testing.T) {
	body := []byte(`{"taskBrief":"x","params":{}}`)
	sig := ComputeSignature("super-secret", body, "hex")
	if !VerifySignature("super-secret", body, sig, "hex") {
		t.Fatal("expected valid hex signature to verify")
	}
}

func TestSignatureHexCaseInsensitive(t *testing.T) {
	body := []byte(`{"taskBrief":"x"}`)
	sig := ComputeSignature("super-secret", body, "hex")
	if !VerifySignature("super-secret", body, strings.ToUpper(sig), "hex") {
		t.Fatal("expected uppercase hex signature to verify case-insensitively")
	}
}

func TestSignatureBase64Roundtrip(t *testing.T) {
	body := []byte("hello mota")
	sig := ComputeSignature("super-secret", body, "base64")
	if !VerifySignature("super-secret", body, sig, "base64") {
		t.Fatal("expected valid base64 signature to verify")
	}
}

func TestSignatureRejectsWrongSecret(t *testing.T) {
	body := []byte("body")
	sig := ComputeSignature("secret-a", body, "hex")
	if VerifySignature("secret-b", body, sig, "hex") {
		t.Fatal("wrong secret must not verify")
	}
}

func TestSignatureRejectsTamperedBody(t *testing.T) {
	sig := ComputeSignature("s3cret", []byte("original"), "hex")
	if VerifySignature("s3cret", []byte("tampered"), sig, "hex") {
		t.Fatal("tampered body must not verify")
	}
}

func TestSignatureRejectsWrongLength(t *testing.T) {
	sig := ComputeSignature("s3cret", []byte("body"), "hex")
	if VerifySignature("s3cret", []byte("body"), sig+"00", "hex") {
		t.Fatal("wrong-length signature must not verify")
	}
}

func TestSignatureRejectsMalformedHex(t *testing.T) {
	sig := ComputeSignature("s3cret", []byte("body"), "hex")
	if VerifySignature("s3cret", []byte("body"), sig[:len(sig)-1]+"z", "hex") {
		t.Fatal("malformed hex signature must not verify")
	}
}

func TestSignatureRejectsMalformedBase64(t *testing.T) {
	if VerifySignature("s3cret", []byte("body"), "not valid base64!!!", "base64") {
		t.Fatal("malformed base64 signature must not verify")
	}
}

func TestSignatureHexAndBase64Differ(t *testing.T) {
	body := []byte("body")
	if ComputeSignature("s", body, "hex") == ComputeSignature("s", body, "base64") {
		t.Fatal("hex and base64 encodings must differ")
	}
}
