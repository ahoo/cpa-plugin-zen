package plugin

import (
	"bytes"
	"context"
	"encoding/json"
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

func freeTestConfig() *pluginConfig {
	return parseConfig([]byte(`
paid:
  api_keys:
    - key: sk-paid
free:
  enabled: true
  models:
    - alias: mimo-free
      name: mimo-v2.6-flash-free
      endpoint: chat
    - alias: muse-free
      name: muse-spark-1.3-contributor-free
      endpoint: responses
  egress:
    - url: http://127.0.0.1:18080
      weight: 10
  quota:
    cooldown: 60
    failover_paid: true
    paid_fallback: deepseek/deepseek-v4.1-flash
`))
}

func TestFreeClaimsPrecedence(t *testing.T) {
	cfg := freeTestConfig()
	for _, name := range []string{"mimo-free", "mimo-v2.6-flash-free", "muse-free", "zen/mimo-free"} {
		if cfg.freeEntry(name) == nil {
			t.Fatalf("%q should resolve to a free entry", name)
		}
	}
	if cfg.freeEntry("deepseek-flash") != nil {
		t.Fatal("deepseek-flash must stay on native channels")
	}
	if cfg.freeEntry("jev") != nil {
		t.Fatal("jev must stay on the paid path")
	}
}

func TestChatCloakForcesAgentShape(t *testing.T) {
	cfg := freeTestConfig()
	raw := []byte(`{"model":"mimo-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	out, err := applyChatCloak(raw, cloakTools(cfg), cloakMinTools(cfg))
	if err != nil {
		t.Fatalf("cloak: %v", err)
	}
	var decoded struct {
		Model      string `json:"model"`
		Stream     bool   `json:"stream"`
		Tools      []any  `json:"tools"`
		ToolChoice string `json:"tool_choice"`
		Messages   []any  `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Stream {
		t.Fatal("cloak must force stream:true")
	}
	if len(decoded.Tools) < 6 {
		t.Fatalf("cloak must top up to 6 tools, got %d", len(decoded.Tools))
	}
	if decoded.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %q", decoded.ToolChoice)
	}
	if decoded.Model != "mimo-free" {
		t.Fatalf("cloak must not rewrite model here, got %q", decoded.Model)
	}
}

func TestChatCloakKeepsClientTools(t *testing.T) {
	cfg := freeTestConfig()
	tools := `[{"type":"function","function":{"name":"a","description":"a","parameters":{"type":"object","properties":{}}}}]`
	raw := []byte(`{"model":"mimo-free","messages":[{"role":"user","content":"hi"}],"tools":` + tools + `}`)
	out, err := applyChatCloak(raw, cloakTools(cfg), cloakMinTools(cfg))
	if err != nil {
		t.Fatalf("cloak: %v", err)
	}
	var decoded struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Tools) != 6 || decoded.Tools[0]["type"] != "function" {
		t.Fatalf("client tool must be preserved first: %+v", decoded.Tools)
	}
}

func TestResponsesBodyConversion(t *testing.T) {
	raw := []byte(`{"model":"muse-free","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}],"max_tokens":20}`)
	out, err := buildResponsesBody(raw, "muse-spark-1.3-contributor-free", []any{}, 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var decoded struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		Stream          bool   `json:"stream"`
		Input           []any  `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Model != "muse-spark-1.3-contributor-free" || !decoded.Stream {
		t.Fatalf("envelope wrong: %+v", decoded)
	}
	if decoded.Instructions != "Be brief." || len(decoded.Input) != 1 || decoded.MaxOutputTokens != 20 {
		t.Fatalf("mapping wrong: %+v", decoded)
	}
}

func TestAssembleChatCompletion(t *testing.T) {
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n")
	out, err := assembleChatCompletion("mimo-free", sse)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Choices[0].Message.Content != "Hello" {
		t.Fatalf("content = %q", decoded.Choices[0].Message.Content)
	}
}

func TestResponsesConverterText(t *testing.T) {
	c := newResponsesConverter()
	line := []byte(`data: {"type":"response.output_text.delta","delta":"Hi"}`)
	out := c.convertLine(line)
	if len(out) == 0 {
		t.Fatal("text delta dropped")
	}
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Choices[0].Delta.Content != "Hi" {
		t.Fatalf("content = %q", decoded.Choices[0].Delta.Content)
	}
	if c.convertLine([]byte(`data: [DONE]`)) != nil {
		t.Fatal("[DONE] must be swallowed")
	}
}

func TestSessionPoolLoadAndCooldown(t *testing.T) {
	p := &sessionPool{sessions: []sessionEntry{{ID: "a"}, {ID: "b"}}}
	if got := p.pick(); got == "" {
		t.Fatal("empty pick on healthy pool")
	}
	p.report("a", false, 3600)
	p.report("b", false, 3600)
	if got := p.pick(); got != "" {
		t.Fatalf("cooled pool must yield nothing, got %q", got)
	}
	p.report("a", true, 0)
	if got := p.pick(); got != "a" {
		t.Fatalf("recovered session not picked: %q", got)
	}
}

func TestFreeMembersMirrorOpencodeEntries(t *testing.T) {
	cfg := parseConfig([]byte(`free:
  enabled: true
  members:
    - key: sk-a
      proxy_url: http://127.0.0.1:18080
      weight: 10
    - key: public
      proxy_url: http://127.0.0.1:18081
    - key: sk-dead
      proxy_url: http://127.0.0.1:18082
      disabled: true
`))
	p := newFreePool(cfg)
	if len(p.members) != 2 {
		t.Fatalf("disabled member must be filtered: %+v", p.members)
	}
	if p.members[0].key != "sk-a" || p.members[0].url != "http://127.0.0.1:18080" {
		t.Fatalf("member not mirrored: %+v", p.members[0])
	}
	if p.members[1].key != "" {
		t.Fatalf("public must normalize to anonymous: %q", p.members[1].key)
	}
	ord := p.order()
	if len(ord) != 2 {
		t.Fatalf("order must cover healthy members: %v", ord)
	}
	p.cool(0, 3600)
	p.cool(1, 3600)
	if ord := p.order(); len(ord) != 0 {
		t.Fatalf("cooled pool must yield nothing: %v", ord)
	}
}

func TestFreeHeadersAuthModes(t *testing.T) {
	anon := freeHeaders("ses_x", "")
	if anon.Get("Authorization") != "Bearer public" {
		t.Fatalf("anonymous must use public: %q", anon.Get("Authorization"))
	}
	keyed := freeHeaders("ses_x", "sk-abc")
	if keyed.Get("Authorization") != "Bearer sk-abc" {
		t.Fatalf("keyed auth wrong: %q", keyed.Get("Authorization"))
	}
	if h := freeHeaders("", ""); h.Get("X-Opencode-Session") != "" {
		t.Fatal("empty session must omit session headers")
	}
}

func TestResponsesMaxOutputTokensClamped(t *testing.T) {
	raw := []byte(`{"model":"muse-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	out, err := buildResponsesBody(raw, "muse-spark-1.3-contributor-free", []any{}, 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var decoded struct {
		MaxOutputTokens int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.MaxOutputTokens != 16 {
		t.Fatalf("max_output_tokens = %d, want clamped 16", decoded.MaxOutputTokens)
	}
}

func TestFreeRetryableGateRotates(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		if !freeRetryable(status, nil) {
			t.Fatalf("status %d must rotate", status)
		}
	}
	for _, status := range []int{400, 404, 422} {
		if freeRetryable(status, nil) {
			t.Fatalf("status %d must fail fast", status)
		}
	}
}

func TestResponsesMinOutputFloor(t *testing.T) {
	entry := FreeModelEntry{MinOutputTokens: 1024}
	raw := []byte(`{"model":"muse-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	out, err := buildResponsesBody(raw, "muse-spark-1.3-contributor-free", []any{}, entry.MinOutputTokens)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var decoded struct {
		MaxOutputTokens int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.MaxOutputTokens != 1024 {
		t.Fatalf("max_output_tokens = %d, want floored 1024", decoded.MaxOutputTokens)
	}
}

func TestEmptyStreamYieldsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := convertResponsesSSE(ctx, []byte("data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n"), framingBare)
	n := 0
	for chunk := range ch {
		if len(bytes.TrimSpace(chunk.Payload)) > 0 {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("converters must not mask emptiness (wrapper owns fallback), got %d chunks", n)
	}
}

func TestIsEmptyCompletion(t *testing.T) {
	if !isEmptyCompletion([]byte(`{"choices":[{"message":{"role":"assistant","content":"  "}}]}`)) {
		t.Fatal("blank content must count as empty")
	}
	if isEmptyCompletion([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)) {
		t.Fatal("real content must not count as empty")
	}
	if isEmptyCompletion([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"1"}]}}]}`)) {
		t.Fatal("tool calls must not count as empty")
	}
	if isEmptyCompletion([]byte(`not-json`)) {
		t.Fatal("unparseable must fail open (non-empty)")
	}
}

func TestStickyAssignStable(t *testing.T) {
	p := &sessionPool{sessions: []sessionEntry{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}}
	a := p.assign("conv-42")
	b := p.assign("conv-42")
	if a == "" || a != b {
		t.Fatalf("assignment must be stable: %q vs %q", a, b)
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.assign("conv-"+string(rune('a'+i)))] = true
	}
	if len(seen) < 2 {
		t.Fatalf("assignments must distribute, got %v", seen)
	}
	p.report(a, false, 3600)
	if got := p.assign("conv-42"); got == a {
		t.Fatalf("cooled session must be skipped, got %q", got)
	}
	if got := p.assign(""); got != "" {
		t.Fatalf("empty downstream must yield nothing, got %q", got)
	}
}

func TestInterceptorStampsPoolSession(t *testing.T) {
	_, p, _ := Build([]byte("free:\n  enabled: true\n"))
	p.executor.sessions.sessions = []sessionEntry{{ID: "s1"}, {ID: "s2"}}
	resp, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestedModel: "mimo-free",
		Headers:        map[string][]string{"Session-Id": {"conv-7"}},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if got := resp.Headers.Get(poolSessionHeader); got == "" {
		t.Fatal("free-bound request with session must be stamped")
	}
	resp2, err := p.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestedModel: "deepseek-flash",
		Headers:        map[string][]string{"Session-Id": {"conv-7"}},
	})
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	if got := resp2.Headers.Get(poolSessionHeader); got != "" {
		t.Fatalf("non-free request must not be stamped, got %q", got)
	}
}

func TestFreeBodyRetryable(t *testing.T) {
	if !freeBodyRetryable([]byte(`{"error":"Endpoint is unavailable"}`)) {
		t.Fatal("unavailable must rotate")
	}
	if freeBodyRetryable([]byte(`{"error":"Model xxx is not supported"}`)) {
		t.Fatal("unsupported model must fail fast")
	}
	if freeBodyRetryable(nil) {
		t.Fatal("empty body must not rotate")
	}
}

func TestStreamFallbackOnEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	empty := make(chan pluginapi.ExecutorStreamChunk)
	close(empty)
	_, p, _ := Build([]byte("free:\n  enabled: true\n"))
	p.executor.cfg.Free.Quota.FailoverPaid = false
	out := p.executor.streamWithPaidFallback(ctx, pluginapi.ExecutorRequest{Model: "muse-free"}, empty, framingBare)
	var chunks [][]byte
	for c := range out {
		chunks = append(chunks, c.Payload)
	}
	if len(chunks) != 1 {
		t.Fatalf("without fallback, empty stream must yield one terminal chunk, got %d", len(chunks))
	}
	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunks[0], &decoded); err != nil {
		t.Fatalf("terminal chunk must be JSON: %v", err)
	}
	if decoded.Model != "muse-free" || len(decoded.Choices) != 1 || decoded.Choices[0].FinishReason != "stop" {
		t.Fatalf("terminal chunk wrong: %+v", decoded)
	}
}

func TestFallbackFloor(t *testing.T) {
	cfg := parseConfig([]byte("paid:\n  api_keys:\n    - key: sk-x\nfree:\n  enabled: true\n  quota:\n    failover_paid: true\n    paid_fallback: deepseek-v4.1-flash\n    fallback_min_tokens: 512\n"))
	if cfg.fallbackFloor() != 512 {
		t.Fatalf("floor = %d", cfg.fallbackFloor())
	}
	def := parseConfig([]byte("paid:\n  api_keys:\n    - key: sk-x\n"))
	if def.fallbackFloor() != 512 {
		t.Fatalf("default floor = %d", def.fallbackFloor())
	}
}
