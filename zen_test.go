package plugin

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testConfig() *pluginConfig {
	return parseConfig([]byte(`
session_mapping:
  enabled: true
paid:
  api_keys:
    - key: sk-test
      weight: 10
  models:
    - alias: jev
      name: jev-1.13
    - alias: jev-free
      name: jev-1.13-free
      questions:
        is_urgent:
          type: noul
          instructions: Urgent?
`))
}

func TestSessionMappingEnabledByDefault(t *testing.T) {
	cfg := parseConfig([]byte(`paid: {}`))
	if !cfg.SessionMapping.effective() {
		t.Fatal("session mapping should default to enabled")
	}
}

func TestSessionAuthoritativeTarget(t *testing.T) {
	_, p, _ := Build(nil)
	resp, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		Headers: map[string][]string{
			"X-Opencode-Session":            {"client-value"},
			"X-DeepSeek-Harness-Session-Id": {"other"},
		},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if len(resp.Headers) != 0 {
		t.Fatalf("authoritative client value must never be overridden: %v", resp.Headers)
	}
}

func TestSessionSourcePriority(t *testing.T) {
	_, p, _ := Build(nil)
	resp, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		Headers: map[string][]string{
			"Session-Id":                    {"codex-1"},
			"X-DeepSeek-Harness-Session-Id": {"dsh-1"},
		},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	got := resp.Headers.Get(targetSessionHeader)
	if got != "codex-1" {
		t.Fatalf("session = %q, want codex-1", got)
	}
	if resp.Headers.Get(targetClientHeader) != defaultClientValue {
		t.Fatalf("client = %q, want %q", resp.Headers.Get(targetClientHeader), defaultClientValue)
	}
}

func TestSessionMetadataFallback(t *testing.T) {
	_, p, _ := Build(nil)
	resp, err := p.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		Headers:  map[string][]string{"User-Agent": {"x"}},
		Metadata: map[string]any{"canonical_session_id": "codex:abc"},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if got := resp.Headers.Get(targetSessionHeader); got != "codex:abc" {
		t.Fatalf("session = %q, want codex:abc", got)
	}
}

func TestSessionConflictingValuesFailClosed(t *testing.T) {
	_, p, _ := Build(nil)
	resp, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		Headers: map[string][]string{"X-Session-Id": {"a", "b"}},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if len(resp.Headers) != 0 {
		t.Fatalf("conflicting values must fail closed: %v", resp.Headers)
	}
}

func TestSessionMappingDisabled(t *testing.T) {
	_, p, _ := Build([]byte("session_mapping:\n  enabled: false\n"))
	resp, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		Headers: map[string][]string{"Session-Id": {"x"}},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if len(resp.Headers) != 0 {
		t.Fatalf("disabled mapping must be a no-op: %v", resp.Headers)
	}
}

func TestPaidModelsClaimed(t *testing.T) {
	cfg := testConfig()
	for _, name := range []string{"jev", "jev-free", "jev-1.13", "jev-1.13-free", "zen/jev-free", "jev-free(high)"} {
		if _, ok := cfg.modelSet()[normalizeModel(name)]; !ok {
			t.Fatalf("%q not claimed", name)
		}
	}
	if got := cfg.upstreamName("jev"); got != "jev-1.13" {
		t.Fatalf("upstream(jev) = %q", got)
	}
}

func TestRouterOwned(t *testing.T) {
	cfg := testConfig()
	r := NewRouter(cfg)
	owned, err := r.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "jev-free"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if !owned.Handled {
		t.Fatal("jev-free should be handled")
	}
	free, err := r.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: "deepseek-flash"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if free.Handled {
		t.Fatal("deepseek-flash must not be handled (falls through to native channels)")
	}
}

func TestEnvelopeNativePath(t *testing.T) {
	cfg := testConfig()
	payload := []byte(`{"model":"jev","messages":[{"role":"user","content":"{\"state\":\"Hi.\",\"questions\":{\"u\":{\"type\":\"noul\",\"instructions\":\"Urgent?\"}}}"}]}`)
	upstream, body, err := buildUpstreamBody("jev", payload, cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if upstream != "jev-1.13" {
		t.Fatalf("upstream = %q", upstream)
	}
	if len(body) == 0 {
		t.Fatal("empty upstream body")
	}
}

func TestMapToCompletion(t *testing.T) {
	raw := []byte(`{"model":"jev-1.13","answers":{"u":{"type":"noul","noul":0.5}},"usage":{"input_tokens":10,"output_tokens":2}}`)
	out, err := mapToCompletion("jev", raw)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty completion")
	}
}
