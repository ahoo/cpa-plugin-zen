package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// systemone.go converts OpenAI chat-completions payloads into the SystemOne
// envelope {model, state, questions} and renders SystemOne answers back as
// standard OpenAI chat completions (message content = JSON-encoded answers).

// MarshalJSON implements json.Marshaler: mapping for choice, list for score.
func (c Criteria) MarshalJSON() ([]byte, error) {
	if len(c.Pairs) > 0 {
		return json.Marshal(c.Pairs)
	}
	if c.List != nil {
		return json.Marshal(c.List)
	}
	return []byte("null"), nil
}

// UnmarshalJSON implements json.Unmarshaler: mapping or sequence.
func (c *Criteria) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "" || strings.TrimSpace(string(data)) == "null" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err == nil {
		c.Pairs = m
		return nil
	}
	var l []string
	if err := json.Unmarshal(data, &l); err != nil {
		return fmt.Errorf("systemone: criteria must be an object or an array of strings")
	}
	c.List = l
	return nil
}

// envelope is the native request shape a client may send as the last user
// message: {"state": "...", "questions": {...}}.
type envelope struct {
	State     string                 `json:"state"`
	Questions map[string]QuestionDef `json:"questions"`
}

// validated converts one question to its upstream shape, failing closed on
// unknown types or missing required fields.
func (q QuestionDef) validated() (map[string]any, error) {
	typ := strings.ToLower(strings.TrimSpace(q.Type))
	if typ != "noul" && typ != "choice" && typ != "score" {
		return nil, fmt.Errorf("systemone: invalid question type %q (want noul|choice|score)", q.Type)
	}
	if strings.TrimSpace(q.Instructions) == "" {
		return nil, fmt.Errorf("systemone: question requires instructions")
	}
	out := map[string]any{"type": typ, "instructions": q.Instructions}
	switch typ {
	case "choice":
		if len(q.Criteria.Pairs) == 0 {
			return nil, fmt.Errorf("systemone: choice question requires a criteria mapping")
		}
		out["criteria"] = q.Criteria.Pairs
	case "score":
		if len(q.Criteria.List) == 0 {
			return nil, fmt.Errorf("systemone: score question requires a criteria list")
		}
		out["criteria"] = q.Criteria.List
	}
	return out, nil
}

// chatText extracts readable text from an OpenAI message content value
// (string or multipart array); non-text parts are skipped.
func chatText(content json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", fmt.Errorf("systemone: unsupported message content shape")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

// parseEnvelope reports whether text is a native {state, questions} envelope.
func parseEnvelope(text string) (envelope, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return envelope{}, false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return envelope{}, false
	}
	if _, ok := probe["questions"]; !ok {
		return envelope{}, false
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return envelope{}, false
	}
	return env, true
}

// upstreamRequest is the body POSTed to /v1/systemone.
type upstreamRequest struct {
	Model     string         `json:"model"`
	State     string         `json:"state"`
	Questions map[string]any `json:"questions"`
}

// buildUpstreamBody maps an OpenAI chat payload to the SystemOne envelope.
// Requested model selects the upstream name and the configured questions.
func buildUpstreamBody(model string, payload []byte, cfg *pluginConfig) (upstreamModel string, body []byte, err error) {
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &chat); err != nil {
		return "", nil, fmt.Errorf("systemone: invalid chat payload: %w", err)
	}
	lastUser := ""
	found := false
	for _, m := range chat.Messages {
		if strings.ToLower(strings.TrimSpace(m.Role)) != "user" {
			continue
		}
		text, err := chatText(m.Content)
		if err != nil {
			return "", nil, err
		}
		lastUser, found = text, true
	}
	if !found || strings.TrimSpace(lastUser) == "" {
		return "", nil, fmt.Errorf("systemone: no user message to use as state")
	}
	var state string
	var questions map[string]QuestionDef
	if env, ok := parseEnvelope(lastUser); ok {
		if strings.TrimSpace(env.State) == "" {
			return "", nil, fmt.Errorf("systemone: envelope requires \"state\"")
		}
		if len(env.Questions) == 0 {
			return "", nil, fmt.Errorf("systemone: envelope requires non-empty \"questions\"")
		}
		state, questions = env.State, env.Questions
	} else {
		state = lastUser
		questions = cfg.questionsFor(model)
		if len(questions) == 0 {
			return "", nil, fmt.Errorf("systemone: no questions for model %q (send a JSON envelope {\"state\":...,\"questions\":{...}} or configure models[].questions)", model)
		}
	}
	upstreamModel = cfg.upstreamName(model)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(model)
	}
	qmap := make(map[string]any, len(questions))
	for id, q := range questions {
		v, err := q.validated()
		if err != nil {
			return "", nil, fmt.Errorf("systemone: question %q: %w", id, err)
		}
		qmap[id] = v
	}
	raw, err := json.Marshal(upstreamRequest{Model: upstreamModel, State: state, Questions: qmap})
	if err != nil {
		return "", nil, fmt.Errorf("systemone: cannot encode upstream request: %w", err)
	}
	return upstreamModel, raw, nil
}

// upstreamResponse is the 200 body returned by /v1/systemone.
type upstreamResponse struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// completionID mints a response id without leaking upstream internals.
func completionID() string {
	return fmt.Sprintf("sys-%d", time.Now().UnixNano())
}

// mapToCompletion renders SystemOne answers as a standard OpenAI chat
// completion; content is the compact JSON-encoded answers object.
func mapToCompletion(requestedModel string, respBody []byte) ([]byte, error) {
	var ur upstreamResponse
	if err := json.Unmarshal(respBody, &ur); err != nil {
		return nil, fmt.Errorf("systemone: invalid upstream response: %w", err)
	}
	if len(bytes.TrimSpace(ur.Answers)) == 0 {
		return nil, fmt.Errorf("systemone: upstream response missing answers")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, ur.Answers); err != nil {
		return nil, fmt.Errorf("systemone: upstream answers are not JSON: %w", err)
	}
	total := ur.Usage.InputTokens + ur.Usage.OutputTokens
	return json.Marshal(map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": compact.String(),
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     ur.Usage.InputTokens,
			"completion_tokens": ur.Usage.OutputTokens,
			"total_tokens":      total,
		},
	})
}

// mapToStreamChunk renders the same answers as a single OpenAI SSE chunk.
func mapToStreamChunk(requestedModel string, respBody []byte) ([]byte, error) {
	var ur upstreamResponse
	if err := json.Unmarshal(respBody, &ur); err != nil {
		return nil, fmt.Errorf("systemone: invalid upstream response: %w", err)
	}
	if len(bytes.TrimSpace(ur.Answers)) == 0 {
		return nil, fmt.Errorf("systemone: upstream response missing answers")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, ur.Answers); err != nil {
		return nil, fmt.Errorf("systemone: upstream answers are not JSON: %w", err)
	}
	return json.Marshal(map[string]any{
		"id":      completionID(),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"content": compact.String(),
			},
			"finish_reason": "stop",
		}},
	})
}
