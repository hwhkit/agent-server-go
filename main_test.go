package main

import (
	"context"
	"net/http"
	"testing"

	kitconfig "github.com/hwhkit/hwhkit-go/config"
)

func redirectRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestSameOriginRedirectPolicy(t *testing.T) {
	original := redirectRequest(t, "https://api.example.com/v1/start")
	if err := sameOriginRedirectPolicy(
		redirectRequest(t, "https://api.example.com/v1/next"),
		[]*http.Request{original},
	); err != nil {
		t.Fatalf("same-origin redirect must be allowed: %v", err)
	}

	for _, target := range []string{
		"https://attacker.example/v1/next",
		"http://api.example.com/v1/next",
		"https://api.example.com:8443/v1/next",
	} {
		if err := sameOriginRedirectPolicy(
			redirectRequest(t, target), []*http.Request{original},
		); err == nil {
			t.Errorf("redirect to %s must be rejected", target)
		}
	}

	via := make([]*http.Request, 10)
	for index := range via {
		via[index] = original
	}
	if err := sameOriginRedirectPolicy(
		redirectRequest(t, "https://api.example.com/v1/loop"), via,
	); err == nil {
		t.Error("redirect limit must be enforced")
	}
}

func TestCredentialedHTTPClientsInstallRedirectPolicy(t *testing.T) {
	clients := map[string]*http.Client{
		"executor": newExecutorHTTPClient(),
		"callback": newCallbackHTTPClient(),
	}
	for name, client := range clients {
		if client.CheckRedirect == nil {
			t.Fatalf("%s client has no redirect policy", name)
		}
		original := redirectRequest(t, "https://api.example.com/start")
		redirected := redirectRequest(t, "https://attacker.example/steal")
		if err := client.CheckRedirect(redirected, []*http.Request{original}); err == nil {
			t.Errorf("%s client accepted a cross-origin redirect", name)
		}
	}
	if timeout := clients["callback"].Timeout; timeout == 0 {
		t.Error("callback client must retain a total request timeout")
	}
}

func TestApplicationBootstrapLoadsRepositoryDefaults(t *testing.T) {
	t.Setenv("HWHKIT_ENV", "")
	t.Setenv("HWHKIT_CONFIG_DIR", "config")
	bootstrap := applicationBootstrap()
	if bootstrap.Env != "default" {
		t.Fatalf("bootstrap env = %q, want default", bootstrap.Env)
	}
	loaded, err := kitconfig.DefaultLoader().Load(context.Background(), bootstrap)
	if err != nil {
		t.Fatalf("load repository defaults: %v", err)
	}
	server := loaded.Config.Server
	if server.BindAddr != "0.0.0.0:8080" {
		t.Errorf("bind addr = %q", server.BindAddr)
	}
	if server.HandlerTimeoutMs != 3610000 || server.WriteTimeoutMs != 3610000 {
		t.Errorf("server timeouts = handler:%d write:%d, want 3610000",
			server.HandlerTimeoutMs, server.WriteTimeoutMs)
	}
}

func TestApplicationBootstrapHonoursExplicitEnvironment(t *testing.T) {
	t.Setenv("HWHKIT_ENV", "staging")
	if got := applicationBootstrap().Env; got != "staging" {
		t.Fatalf("bootstrap env = %q, want staging", got)
	}
}
