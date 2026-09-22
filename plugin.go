// Package plugin implements the Zen suite for CLIProxyAPI: OpenCode Zen
// session forwarding plus direct (paid-key) providers.
//
// Phase 1: request_interceptor (X-Opencode-Session mapping, ported from
// cpa-plugin-opencode-session-mapper) + ModelProvider + ModelRouter +
// Executor over the SystemOne endpoint (ported from cpa-plugin-systemone).
// Free-tier pools arrive in Phase 2 and never share the paid keys.
package plugin

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// Provider is the executor/model provider key. It must not collide with
	// any built-in provider key; native executors always win on collision.
	// NOTE: Phase 1 renames systemone/* model IDs to zen/*. Clients keep
	// requesting the bare aliases (jev, jev-free), which are unchanged.
	Provider = "zen"

	// executorFormat declares the semantic payload this executor consumes and
	// emits. Both are OpenAI chat-completions JSON; the host translates to/from
	// claude/openai-responses around us.
	executorFormat = "openai"

	// upstreamBaseURL is the OpenCode Zen endpoint root.
	upstreamBaseURL = "https://opencode.ai/zen"
)

// pluginVersion tracks the release; cmd/zen/abi.go carries its own copy for
// registration metadata (injected via ldflags at release time).
var pluginVersion = "0.1.0"

// ZenPlugin wires session mapping, model metadata, routing, translation and
// execution. One handler backs every capability.
type ZenPlugin struct {
	models     *ModelProvider
	router     *Router
	translator *Translator
	executor   *Executor
	cfg        *pluginConfig
}

// Build constructs the host-facing plugin description from the raw
// plugins.configs.zen YAML the host passes at register/reconfigure time.
// An unparseable non-empty stanza fails loudly instead of degrading to a
// keyless default configuration.
func Build(configYAML []byte) (pluginapi.Plugin, *ZenPlugin, error) {
	cfg, err := parseConfigStrict(configYAML)
	if err != nil {
		return pluginapi.Plugin{}, nil, err
	}
	p := &ZenPlugin{
		models: NewModelProvider(cfg),
		cfg:    cfg,
	}
	p.router = NewRouter(cfg)
	p.translator = NewTranslator(cfg)
	p.executor = NewExecutor(cfg)
	desc := pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             "Zen Suite",
			Version:          pluginVersion,
			Author:           "ahoo",
			GitHubRepository: "https://github.com/ahoo/cpa-plugin-zen",
		},
		Capabilities: pluginapi.Capabilities{
			RequestInterceptor:    p,
			ModelProvider:         p.models,
			ModelRouter:           p.router,
			Executor:              p.executor,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{executorFormat},
			ExecutorOutputFormats: []string{executorFormat},
			RequestTranslator:     p.translator,
			ResponseTranslator:    p.translator,
		},
	}
	return desc, p, nil
}

// Identifier returns the provider key.
func (p *ZenPlugin) Identifier() string { return Provider }

// InterceptRequestBeforeAuth maps session headers before credential selection.
func (p *ZenPlugin) InterceptRequestBeforeAuth(ctx context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	_ = ctx
	return p.interceptSession(req)
}

// InterceptRequestAfterAuth maps session headers after credential selection
// (this pass carries the host-computed canonical_session_id fallback).
func (p *ZenPlugin) InterceptRequestAfterAuth(ctx context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	_ = ctx
	return p.interceptSession(req)
}

func (p *ZenPlugin) interceptSession(req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if p.cfg == nil || !p.cfg.SessionMapping.effective() {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	mapped := mapSessionHeaders(map[string][]string(req.Headers), req.Metadata)
	if len(mapped) == 0 {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	return pluginapi.RequestInterceptResponse{Headers: mapped}, nil
}

// StaticModels returns the Zen direct models served through this executor.
func (p *ZenPlugin) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.StaticModels(ctx, req)
}

// ModelsForAuth mirrors static models; Zen auths live in the plugin config
// pool, so per-auth discovery is a no-op.
func (p *ZenPlugin) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.ModelsForAuth(ctx, req)
}

// RouteModel hijacks the Zen-owned models to this plugin's executor.
func (p *ZenPlugin) RouteModel(ctx context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	return p.router.RouteModel(ctx, req)
}

// TranslateRequest converts canonical requests (model-name normalization).
func (p *ZenPlugin) TranslateRequest(ctx context.Context, req pluginapi.RequestTransformRequest) (pluginapi.PayloadResponse, error) {
	return p.translator.TranslateRequest(ctx, req)
}

// TranslateResponse passes executor output through (already OpenAI shape).
func (p *ZenPlugin) TranslateResponse(ctx context.Context, req pluginapi.ResponseTransformRequest) (pluginapi.PayloadResponse, error) {
	return p.translator.TranslateResponse(ctx, req)
}

// Execute performs a non-streaming upstream call.
func (p *ZenPlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.Execute(ctx, req)
}

// ExecuteStream performs the single-shot upstream call and emits it as one
// stream chunk (SystemOne has no streaming protocol of its own).
func (p *ZenPlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return p.executor.ExecuteStream(ctx, req)
}

// CountTokens estimates tokens without calling upstream.
func (p *ZenPlugin) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.CountTokens(ctx, req)
}

// HttpRequest bridges executor-owned raw HTTP through the host client.
func (p *ZenPlugin) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return p.executor.HttpRequest(ctx, req)
}

var (
	_ pluginapi.RequestInterceptor = (*ZenPlugin)(nil)
	_ pluginapi.ModelProvider      = (*ZenPlugin)(nil)
	_ pluginapi.ModelRouter        = (*ZenPlugin)(nil)
	_ pluginapi.RequestTranslator  = (*ZenPlugin)(nil)
	_ pluginapi.ResponseTranslator = (*ZenPlugin)(nil)
	_ pluginapi.ProviderExecutor   = (*ZenPlugin)(nil)
)

// normalizeModel strips provider prefixes, alias suffixes and whitespace so
// "jev-free", "zen/jev-free" and "jev-1.13-free(high)" compare equal
// downstream.
func normalizeModel(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "("); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return strings.ToLower(m)
}
