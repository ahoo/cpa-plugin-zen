package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Executor forwards OpenAI chat-completions payloads to the SystemOne
// endpoint as {model, state, questions} and renders the structured answers
// back into standard OpenAI shape.
//
// Requests go through the weighted multi-key pool. Members without proxy_url
// use the host HTTP client (host proxy policy + request-log preserved);
// members with proxy_url use a self-built transport. Failover retries
// 429/5xx/transport errors on the next member.
type Executor struct {
	cfg      *pluginConfig
	keypool  *pool
	sessions *sessionPool
	egress   *egressPool
}

func NewExecutor(cfg *pluginConfig) *Executor {
	return &Executor{
		cfg:      cfg,
		keypool:  newPool(),
		sessions: loadSessionPool(sessionPoolPath(cfg)),
		egress:   newEgressPool(cfg),
	}
}

// sessionPoolPath resolves the pool file (absolute or host-relative).
func sessionPoolPath(cfg *pluginConfig) string {
	if cfg != nil && strings.TrimSpace(cfg.Free.SessionPool) != "" {
		return strings.TrimSpace(cfg.Free.SessionPool)
	}
	return "plugins/zen-sessions.json"
}

func (e *Executor) Identifier() string { return Provider }

const missingKeyMsg = "zen executor: missing api key (router path passes nil auth; set plugins.configs.zen.api_key or api_keys in config.yaml)"

const claudeMessagesPath = "/v1/messages"

func upstreamHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "cli-proxy-zen")
	h.Set("Accept", "application/json")
	return h
}

func (e *Executor) endpoint() string {
	return strings.TrimSuffix(e.cfg.baseURL(), "/") + "/v1/systemone"
}

// call performs one single-shot upstream call and maps the answers to an
// OpenAI chat completion.
func (e *Executor) call(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	members := e.cfg.members(req)
	if len(members) == 0 {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	_, sysBody, err := buildUpstreamBody(req.Model, req.Payload, e.cfg)
	if err != nil {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadRequest, msg: err.Error()}
	}
	var lastErr error
	for _, idx := range e.keypool.order(members) {
		m := members[idx]
		d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
		if err != nil {
			lastErr = err
			continue
		}
		status, headers, respBody, err := d.do(ctx, e.endpoint(), upstreamHeaders(strings.TrimSpace(m.Key)), sysBody)
		if err != nil {
			lastErr = err
			if retryable(0, err) && ctx.Err() == nil {
				continue
			}
			return pluginapi.ExecutorResponse{}, err
		}
		if status < 200 || status >= 300 {
			lastErr = statusError{statusCode: status, body: respBody}
			if retryable(status, nil) && ctx.Err() == nil {
				continue
			}
			return pluginapi.ExecutorResponse{}, lastErr
		}
		mapped, err := mapToCompletion(req.Model, respBody)
		if err != nil {
			return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadGateway, msg: err.Error()}
		}
		return pluginapi.ExecutorResponse{Payload: mapped, Headers: headers}, nil
	}
	if lastErr != nil {
		return pluginapi.ExecutorResponse{}, lastErr
	}
	return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "zen executor: all pool members failed"}
}

// Execute performs a non-streaming call, routing free models to the free
// path and everything else to the paid pool.
func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if entry := e.cfg.freeEntry(req.Model); entry != nil {
		return e.executeFree(ctx, req, *entry)
	}
	return e.call(ctx, req)
}

// ExecuteStream performs the same single-shot call and emits the answers as
// one stream chunk. Normalized chunks stay bare for OpenAI routes;
// /v1/messages receives one data: prefix because the host's OpenAI-to-Claude
// translator consumes SSE-framed input. The host emits its own stream tail.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	if entry := e.cfg.freeEntry(req.Model); entry != nil {
		return e.executeFreeStream(ctx, req, *entry)
	}
	members := e.cfg.members(req)
	if len(members) == 0 {
		return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	_, sysBody, err := buildUpstreamBody(req.Model, req.Payload, e.cfg)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusBadRequest, msg: err.Error()}
	}
	var lastErr error
	for _, idx := range e.keypool.order(members) {
		m := members[idx]
		d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
		if err != nil {
			lastErr = err
			continue
		}
		status, headers, respBody, err := d.do(ctx, e.endpoint(), upstreamHeaders(strings.TrimSpace(m.Key)), sysBody)
		if err != nil {
			lastErr = err
			if retryable(0, err) && ctx.Err() == nil {
				continue
			}
			return pluginapi.ExecutorStreamResponse{}, err
		}
		if status < 200 || status >= 300 {
			lastErr = statusError{statusCode: status, body: respBody}
			if retryable(status, nil) && ctx.Err() == nil {
				continue
			}
			return pluginapi.ExecutorStreamResponse{}, lastErr
		}
		chunk, err := mapToStreamChunk(req.Model, respBody)
		if err != nil {
			return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusBadGateway, msg: err.Error()}
		}
		if framingForRequest(req) == framingClaude {
			chunk = append([]byte("data: "), chunk...)
		}
		ch := make(chan pluginapi.ExecutorStreamChunk, 1)
		ch <- pluginapi.ExecutorStreamChunk{Payload: chunk}
		close(ch)
		return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: ch}, nil
	}
	if lastErr != nil {
		return pluginapi.ExecutorStreamResponse{}, lastErr
	}
	return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "zen executor: all pool members failed"}
}

type streamFraming uint8

const (
	framingBare streamFraming = iota
	framingClaude
)

// framingForRequest reads the host's public request_path metadata. Only an
// exact /v1/messages value selects Claude framing; everything else stays
// bare (fail closed, prevents "data: data:" output).
func framingForRequest(req pluginapi.ExecutorRequest) streamFraming {
	if path, ok := req.Metadata[coreexecutor.RequestPathMetadataKey].(string); ok && path == claudeMessagesPath {
		return framingClaude
	}
	return framingBare
}

// CountTokens is a local estimate; SystemOne exposes no tokenize endpoint.
func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	_ = ctx
	count := int64(len(req.Payload) / 4)
	if count < 1 && len(req.Payload) > 0 {
		count = 1
	}
	raw, _ := json.Marshal(map[string]any{
		"id":      "zen-count",
		"object":  "chat.completion",
		"created": 0,
		"model":   req.Model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     count,
			"completion_tokens": 0,
			"total_tokens":      count,
		},
	})
	return pluginapi.ExecutorResponse{Payload: raw}, nil
}

// HttpRequest bridges raw executor HTTP through the pool: first member's
// transport with the resolved api key injected when the caller did not set
// Authorization.
func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("zen executor: request URL is required")
	}
	headers := req.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	members := e.cfg.members(poolReqFromHTTP(req))
	if len(members) == 0 && headers.Get("Authorization") == "" {
		return pluginapi.ExecutorHTTPResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	if headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(members[0].Key))
	}
	var d doer
	if len(members) > 0 {
		var err error
		d, err = e.keypool.clientFor(0, members[0], req.HTTPClient)
		if err != nil {
			return pluginapi.ExecutorHTTPResponse{}, err
		}
	} else {
		if req.HTTPClient == nil {
			return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("zen executor: host HTTP client is required")
		}
		d = hostDoer{client: req.HTTPClient}
	}
	status, respHeaders, respBody, err := d.do(ctx, strings.TrimSpace(req.URL), headers, req.Body)
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: status, Headers: respHeaders, Body: respBody}, nil
}

// statusError carries an upstream HTTP status back to the host (the ABI
// error envelope preserves it as http_status for retry classification).
type statusError struct {
	statusCode int
	msg        string
	body       []byte
}

func (e statusError) Error() string {
	if strings.TrimSpace(e.msg) != "" {
		return e.msg
	}
	if len(e.body) > 0 {
		return upstreamErrorMessage(e.body)
	}
	return fmt.Sprintf("status %d", e.statusCode)
}

func (e statusError) StatusCode() int { return e.statusCode }

func upstreamErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		if len(decoded.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if errObj := json.Unmarshal(decoded.Error, &obj); errObj == nil && strings.TrimSpace(obj.Message) != "" {
				return strings.TrimSpace(obj.Message)
			}
			var s string
			if errStr := json.Unmarshal(decoded.Error, &s); errStr == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if strings.TrimSpace(decoded.Message) != "" {
			return strings.TrimSpace(decoded.Message)
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500]
	}
	return trimmed
}
