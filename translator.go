package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Translator converts between host canonical formats and the SystemOne
// envelope. The executor consumes OpenAI chat-completions directly, so the
// request direction only normalizes the model name and the response
// direction passes executor output through (already OpenAI shape).
type Translator struct {
	cfg *pluginConfig
}

func NewTranslator(cfg *pluginConfig) *Translator { return &Translator{cfg: cfg} }

// TranslateRequest handles canonical -> systemone. Only the model field is
// rewritten (client alias -> upstream name); the payload is otherwise
// untouched for the executor to map.
func (t *Translator) TranslateRequest(ctx context.Context, req pluginapi.RequestTransformRequest) (pluginapi.PayloadResponse, error) {
	_ = ctx
	from := strings.ToLower(strings.TrimSpace(req.FromFormat))
	to := strings.ToLower(strings.TrimSpace(req.ToFormat))
	if to != "" && to != "systemone" && to != "openai" {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported request translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	switch from {
	case "", "openai", "systemone":
		return pluginapi.PayloadResponse{Body: t.normalizeRequestModel(req.Model, req.Body)}, nil
	default:
		return pluginapi.PayloadResponse{Body: append([]byte(nil), req.Body...)}, nil
	}
}

// TranslateResponse handles systemone -> canonical (pass-through: executor
// output is already standard OpenAI).
func (t *Translator) TranslateResponse(ctx context.Context, req pluginapi.ResponseTransformRequest) (pluginapi.PayloadResponse, error) {
	_ = ctx
	from := strings.ToLower(strings.TrimSpace(req.FromFormat))
	to := strings.ToLower(strings.TrimSpace(req.ToFormat))
	if from != "" && from != "systemone" && from != "openai" {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported response translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	if to != "" && to != "openai" && to != "claude" && to != "openai-response" && to != "systemone" {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported response translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	return pluginapi.PayloadResponse{Body: append([]byte(nil), req.Body...)}, nil
}

// normalizeRequestModel rewrites the outbound model to the upstream name.
func (t *Translator) normalizeRequestModel(model string, body []byte) []byte {
	upstream := t.cfg.upstreamName(model)
	if upstream == "" || len(body) == 0 {
		return append([]byte(nil), body...)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return append([]byte(nil), body...)
	}
	decoded["model"] = upstream
	raw, err := json.Marshal(decoded)
	if err != nil {
		return append([]byte(nil), body...)
	}
	return raw
}
