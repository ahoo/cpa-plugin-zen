package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	mrand "math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// free.go implements the anonymous free-tier path: pooled sessions +
// agent-shape cloak + egress pool with cooldown + paid fallback.

// defaultFreeTools are the validated generic dev tools topping cloak bodies
// up to min_tools (6× generic validated 200 against mimo-free).
func defaultFreeTools() []map[string]any {
	names := []string{"bash", "read", "edit", "write", "glob", "grep"}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        n,
				"description": "developer tool",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return out
}

func cloakMinTools(cfg *pluginConfig) int {
	if cfg != nil && cfg.Free.Cloak.MinTools > 0 {
		return cfg.Free.Cloak.MinTools
	}
	return 6
}

func cloakTools(cfg *pluginConfig) []map[string]any {
	if cfg != nil && len(cfg.Free.Cloak.Tools) > 0 {
		return cfg.Free.Cloak.Tools
	}
	return defaultFreeTools()
}

// sessionPool is the file-backed pool of genuinely-minted session IDs.
type sessionEntry struct {
	ID            string `json:"id"`
	Fails         int    `json:"fails"`
	CooldownUntil int64  `json:"cooldown_until"`
}

type sessionPool struct {
	mu       sync.Mutex
	sessions []sessionEntry
	rr       int
}

func loadSessionPool(path string) *sessionPool {
	p := &sessionPool{}
	if strings.TrimSpace(path) == "" {
		return p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var doc struct {
		Sessions []sessionEntry `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return p
	}
	for _, s := range doc.Sessions {
		if strings.TrimSpace(s.ID) != "" {
			p.sessions = append(p.sessions, s)
		}
	}
	return p
}

func (p *sessionPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// pick returns the next healthy session id (round-robin, skipping cooled
// down and repeatedly failed entries), or "" when the pool is exhausted.
func (p *sessionPool) pick() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	for i := 0; i < len(p.sessions); i++ {
		p.rr = (p.rr + 1) % len(p.sessions)
		s := &p.sessions[p.rr]
		if s.Fails >= 5 || s.CooldownUntil > now {
			continue
		}
		return s.ID
	}
	return ""
}

// report records one use outcome for failover rotation.
func (p *sessionPool) report(id string, ok bool, cooldownSec int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.sessions {
		if p.sessions[i].ID != id {
			continue
		}
		if ok {
			p.sessions[i].Fails = 0
			p.sessions[i].CooldownUntil = 0
		} else {
			p.sessions[i].Fails++
			if cooldownSec > 0 {
				p.sessions[i].CooldownUntil = time.Now().Unix() + int64(cooldownSec)
			}
		}
		return
	}
}

// poolSessionHeader carries the sticky pool assignment from the
// interceptor to the executor. Internal only: executors build their own
// upstream headers and never forward it.
const poolSessionHeader = "X-Zen-Pool-Session"

// assign binds a downstream conversation identity to one pool session
// (stable hash, forward probe past cooled/failed entries). Empty when the
// pool has no usable session.
func (p *sessionPool) assign(downstream string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.sessions)
	if n == 0 || strings.TrimSpace(downstream) == "" {
		return ""
	}
	now := time.Now().Unix()
	start := int(fnvHash(downstream) % uint64(n))
	for i := 0; i < n; i++ {
		s := &p.sessions[(start+i)%n]
		if s.Fails < 5 && s.CooldownUntil <= now {
			return s.ID
		}
	}
	return ""
}

func fnvHash(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// egressPool rotates exit proxies for free traffic (trial quota is
// IP-bound). Cooldowns live in memory; disabled members are filtered at
// build time.
type freeLiveMember struct {
	key           string
	url           string
	weight        int
	idx           int
	cooldownUntil time.Time
	fails         int
}

type freePool struct {
	mu      sync.Mutex
	rnd     *lockedRand
	members []freeLiveMember
	clients map[int]*http.Client
}

func newFreePool(cfg *pluginConfig) *freePool {
	p := &freePool{rnd: &lockedRand{}, clients: map[int]*http.Client{}}
	if cfg == nil {
		return p
	}
	for i, en := range cfg.Free.Members {
		if en.Disabled {
			continue
		}
		p.members = append(p.members, freeLiveMember{key: en.authKey(), url: strings.TrimSpace(en.ProxyURL), weight: en.normWeight(), idx: i})
	}
	return p
}

// order returns member indices in weighted-random order, skipping cooled
// down members (they rejoin automatically on expiry).
func (p *freePool) order() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	remaining := make([]int, 0, len(p.members))
	for i := range p.members {
		if p.members[i].cooldownUntil.After(now) {
			continue
		}
		remaining = append(remaining, i)
	}
	out := make([]int, 0, len(remaining))
	for len(remaining) > 0 {
		weights := 0
		for _, i := range remaining {
			weights += p.members[i].weight
		}
		r := p.rnd.intn(weights)
		pick := 0
		for i, idx := range remaining {
			r -= p.members[idx].weight
			if r < 0 {
				pick = i
				break
			}
		}
		out = append(out, remaining[pick])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

func (p *freePool) cool(idx int, cooldownSec int) {
	if cooldownSec <= 0 {
		cooldownSec = 300
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx >= 0 && idx < len(p.members) {
		p.members[idx].cooldownUntil = time.Now().Add(time.Duration(cooldownSec) * time.Second)
		p.members[idx].fails++
	}
}

func (p *freePool) clientFor(idx int, hostClient pluginapi.HostHTTPClient) (doer, error) {
	if idx < 0 {
		if hostClient == nil {
			return nil, fmt.Errorf("zen free executor: host HTTP client is required")
		}
		return hostDoer{client: hostClient}, nil
	}
	url := p.members[idx].url
	if url == "" {
		if hostClient == nil {
			return nil, fmt.Errorf("zen free executor: host HTTP client is required")
		}
		return hostDoer{client: hostClient}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[idx]; ok {
		return stdDoer{client: c}, nil
	}
	transport, err := proxyTransport(url)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: transport, Timeout: 100 * time.Second}
	p.clients[idx] = c
	return stdDoer{client: c}, nil
}

type lockedRand struct {
	mu  sync.Mutex
	rnd *mrand.Rand
}

func (r *lockedRand) intn(n int) int {
	if n <= 1 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rnd == nil {
		r.rnd = mrand.New(mrand.NewSource(time.Now().UnixNano()))
	}
	return r.rnd.Intn(n)
}

// requestPayload returns the translated provider payload, falling back to
// the raw client body when translation yields nothing parseable.
func requestPayload(req pluginapi.ExecutorRequest) []byte {
	if len(bytes.TrimSpace(req.Payload)) > 0 {
		var probe struct {
			Messages []any `json:"messages"`
		}
		if json.Unmarshal(req.Payload, &probe) == nil && len(probe.Messages) > 0 {
			return req.Payload
		}
	}
	return req.OriginalRequest
}

// freeHeaders builds the genuine-CLI header set for free calls. Empty key
// selects the anonymous tier (Bearer public); otherwise the logged-in key.
func freeHeaders(sessionID, key string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	if strings.TrimSpace(key) == "" {
		h.Set("Authorization", "Bearer public")
	} else {
		h.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	}
	h.Set("User-Agent", "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
	h.Set("X-Opencode-Client", "cli")
	h.Set("X-Opencode-Project", "global")
	if strings.TrimSpace(sessionID) != "" {
		h.Set("X-Opencode-Session", sessionID)
		h.Set("X-Opencode-Request", "msg_"+randomSuffix(12))
	}
	return h
}

func randomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			out[i] = chars[i%len(chars)]
			continue
		}
		out[i] = chars[v.Int64()]
	}
	return string(out)
}

// applyChatCloak forces the agent shape: stream + topped-up tools +
// tool_choice auto. Returns the upstream model (unchanged for chat).
func applyChatCloak(payload []byte, tools []map[string]any, minTools int) ([]byte, error) {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("zen free executor: invalid chat payload: %w", err)
	}
	decoded["stream"] = true
	existing, _ := decoded["tools"].([]any)
	if len(existing) < minTools {
		merged := make([]any, 0, len(existing)+len(tools))
		merged = append(merged, existing...)
		for _, t := range tools {
			if len(merged) >= minTools {
				break
			}
			merged = append(merged, t)
		}
		decoded["tools"] = merged
	}
	if _, ok := decoded["tool_choice"]; !ok {
		decoded["tool_choice"] = "auto"
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("zen free executor: cannot encode cloaked request: %w", err)
	}
	return raw, nil
}

// buildResponsesBody converts an OpenAI chat payload to a Responses request.
// tools must already be topped up by the caller.
func buildResponsesBody(payload []byte, upstreamModel string, tools []any, minOutput int) ([]byte, error) {
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(payload, &chat); err != nil {
		return nil, fmt.Errorf("zen free executor: invalid chat payload: %w", err)
	}
	if len(chat.Messages) == 0 {
		return nil, fmt.Errorf("zen free executor: no messages to convert")
	}
	var instructions string
	var input []any
	for _, m := range chat.Messages {
		text, err := chatText(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "system", "developer":
			if instructions == "" {
				instructions = text
			} else {
				instructions += "\n" + text
			}
		default:
			input = append(input, map[string]any{"role": m.Role, "content": text})
		}
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("zen free executor: no input messages to convert")
	}
	out := map[string]any{
		"model":  upstreamModel,
		"input":  input,
		"stream": true,
		"tools":  flattenResponsesTools(tools),
	}
	if instructions != "" {
		out["instructions"] = instructions
	}
	if chat.MaxTokens > 0 {
		out["max_output_tokens"] = chat.MaxTokens
	} else {
		out["max_output_tokens"] = 1024
	}
	// Upstream rejects max_output_tokens < 16; thinking models additionally
	// burn budget on reasoning, so floor small requests to leave room for
	// an answer instead of ending the stream empty.
	floor := minOutput
	if floor <= 0 {
		floor = 16
	}
	if n, ok := out["max_output_tokens"].(int); ok && n < floor {
		out["max_output_tokens"] = floor
	}
	return json.Marshal(out)
}

// flattenResponsesTools converts OpenAI function tools
// ({type:function,function:{...}}) to the flat Responses shape
// ({type:function,name,description,parameters,...}).
func flattenResponsesTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok || m["type"] != "function" {
			out = append(out, t)
			continue
		}
		flat := map[string]any{"type": "function"}
		for k, v := range fn {
			flat[k] = v
		}
		for k, v := range m {
			if k != "function" && k != "type" {
				flat[k] = v
			}
		}
		out = append(out, flat)
	}
	return out
}

// assembleChatCompletion folds OpenAI SSE data lines into one completion.
func assembleChatCompletion(requestedModel string, sse []byte) ([]byte, error) {
	var content strings.Builder
	finish := "stop"
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	for _, line := range bytes.Split(sse, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		for bytes.HasPrefix(bytes.TrimSpace(payload), []byte("data:")) {
			p := bytes.TrimSpace(payload)
			payload = bytes.TrimSpace(p[len("data:"):])
		}
		if len(payload) == 0 || string(payload) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
		if chunk.Usage != nil {
			usage["prompt_tokens"] = chunk.Usage.PromptTokens
			usage["completion_tokens"] = chunk.Usage.CompletionTokens
			usage["total_tokens"] = chunk.Usage.TotalTokens
		}
	}
	return json.Marshal(map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content.String()},
			"finish_reason": finish,
		}},
		"usage": usage,
	})
}

// responsesConverter turns Responses SSE into OpenAI deltas incrementally.
type responsesConverter struct {
	indexByItem map[string]int
	names       map[string]string
	nextIndex   int
}

func newResponsesConverter() *responsesConverter {
	return &responsesConverter{indexByItem: map[string]int{}, names: map[string]string{}}
}

func (c *responsesConverter) convertLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	for bytes.HasPrefix(bytes.TrimSpace(payload), []byte("data:")) {
		p := bytes.TrimSpace(payload)
		payload = bytes.TrimSpace(p[len("data:"):])
	}
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return nil
	}
	var ev struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Item  *struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
		ItemID   string `json:"item_id"`
		Response *struct {
			Status string `json:"status"`
			Usage  *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil
		}
		return marshalDelta(map[string]any{"role": "assistant", "content": ev.Delta}, "", nil)
	case "response.output_item.added":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return nil
		}
		idx, ok := c.indexByItem[ev.Item.ID]
		if !ok {
			idx = c.nextIndex
			c.nextIndex++
			c.indexByItem[ev.Item.ID] = idx
		}
		c.names[ev.Item.ID] = ev.Item.Name
		return marshalDelta(nil, "", []any{map[string]any{
			"index": idx, "id": ev.Item.ID, "type": "function",
			"function": map[string]any{"name": ev.Item.Name, "arguments": ""},
		}})
	case "response.function_call_arguments.delta":
		idx, ok := c.indexByItem[ev.ItemID]
		if !ok || ev.Delta == "" {
			return nil
		}
		return marshalDelta(nil, "", []any{map[string]any{
			"index": idx, "function": map[string]any{"arguments": ev.Delta},
		}})
	case "response.completed":
		finish := "stop"
		usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
		if ev.Response != nil {
			if ev.Response.Status == "failed" || ev.Response.Status == "cancelled" {
				finish = "stop"
			}
			if ev.Response.Usage != nil {
				usage["prompt_tokens"] = ev.Response.Usage.InputTokens
				usage["completion_tokens"] = ev.Response.Usage.OutputTokens
				usage["total_tokens"] = ev.Response.Usage.InputTokens + ev.Response.Usage.OutputTokens
			}
		}
		raw, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
			"usage":   usage,
		})
		return raw
	default:
		return nil
	}
}

func marshalDelta(delta map[string]any, finish string, toolCalls []any) []byte {
	choice := map[string]any{"index": 0}
	if delta != nil {
		choice["delta"] = delta
	} else {
		choice["delta"] = map[string]any{}
	}
	if toolCalls != nil {
		d := choice["delta"].(map[string]any)
		d["tool_calls"] = toolCalls
	}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{choice}})
	return raw
}

// assembleResponsesCompletion folds Responses SSE into one OpenAI completion.
func assembleResponsesCompletion(requestedModel string, sse []byte) ([]byte, error) {
	c := newResponsesConverter()
	var content strings.Builder
	toolArgs := map[int]string{}
	toolNames := map[int]string{}
	toolIDs := map[int]string{}
	finish := "stop"
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	for _, line := range bytes.Split(sse, []byte("\n")) {
		out := c.convertLine(line)
		if len(out) == 0 {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
				Usage        map[string]any `json:"usage"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(out, &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			content.WriteString(ch.Delta.Content)
			for _, tc := range ch.Delta.ToolCalls {
				if tc.ID != "" {
					toolIDs[tc.Index] = tc.ID
				}
				if tc.Function.Name != "" {
					toolNames[tc.Index] = tc.Function.Name
				}
				toolArgs[tc.Index] += tc.Function.Arguments
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
			for k, v := range ch.Usage {
				usage[k] = v
			}
		}
		if chunk.Usage != nil {
			for k, v := range chunk.Usage {
				usage[k] = v
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if len(toolArgs) > 0 {
		var calls []any
		for idx := 0; idx < len(toolArgs)+len(toolNames)+len(toolIDs)+1; idx++ {
			if _, ok := toolArgs[idx]; !ok {
				if _, ok := toolNames[idx]; !ok {
					continue
				}
			}
			calls = append(calls, map[string]any{
				"id":   toolIDs[idx],
				"type": "function",
				"function": map[string]any{
					"name":      toolNames[idx],
					"arguments": toolArgs[idx],
				},
			})
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	return json.Marshal(map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	})
}

// freeUpstreamPath maps endpoint kinds to Zen paths.
func freeUpstreamPath(endpoint string) string {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "responses":
		return "/v1/responses"
	case "systemone":
		return "/v1/systemone"
	default:
		return "/v1/chat/completions"
	}
}

// freeAttempt performs one (session, egress) upstream call. It returns the
// raw upstream body for the caller to map.
func (e *Executor) freeAttempt(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry, session string, member freeLiveMember, memberIdx int) (int, http.Header, []byte, error) {
	base := strings.TrimSuffix(e.cfg.baseURL(), "/")
	url := base + freeUpstreamPath(entry.Endpoint)
	tools := cloakTools(e.cfg)
	minTools := cloakMinTools(e.cfg)
	payload := requestPayload(req)
	var body []byte
	var err error
	switch strings.ToLower(strings.TrimSpace(entry.Endpoint)) {
	case "responses":
		existing := existingTools(payload)
		merged := mergeTools(existing, tools, minTools)
		floor := entry.MinOutputTokens
		if floor <= 0 {
			floor = 1024
		}
		body, err = buildResponsesBody(payload, strings.TrimSpace(entry.Name), merged, floor)
	case "systemone":
		var upstream string
		upstream, body, err = buildUpstreamBody(req.Model, payload, e.cfg)
		_ = upstream
		if err == nil {
			// Free aliases never rewrite via the shared table; resolve
			// the vendor name from the free entry itself.
			if name := strings.TrimSpace(entry.Name); name != "" {
				body, err = setBodyModel(body, name, nil)
			}
		}
	default:
		body, err = applyChatCloak(payload, tools, minTools)
		if upstream := strings.TrimSpace(entry.Name); upstream != "" {
			body, err = setBodyModel(body, upstream, err)
		}
	}
	if err != nil {
		return 0, nil, nil, err
	}
	headers := freeHeaders(session, member.key)
	var d doer
	if strings.TrimSpace(member.url) == "" {
		if req.HTTPClient == nil {
			return 0, nil, nil, fmt.Errorf("zen free executor: host HTTP client is required")
		}
		d = hostDoer{client: req.HTTPClient}
	} else {
		var err error
		d, err = e.freepool.clientFor(memberIdx, req.HTTPClient)
		if err != nil {
			return 0, nil, nil, err
		}
	}
	return d.do(ctx, url, headers, body)
}

func setBodyModel(body []byte, model string, prev error) ([]byte, error) {
	if prev != nil {
		return nil, prev
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	decoded["model"] = model
	return json.Marshal(decoded)
}

func existingTools(payload []byte) []any {
	var decoded struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil
	}
	return decoded.Tools
}

func mergeTools(existing []any, cloak []map[string]any, minTools int) []any {
	merged := make([]any, 0, len(existing)+len(cloak))
	merged = append(merged, existing...)
	for _, t := range cloak {
		if len(merged) >= minTools {
			break
		}
		merged = append(merged, t)
	}
	return merged
}

// executeFree runs the anonymous free path with session×egress failover.
func (e *Executor) executeFree(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry) (pluginapi.ExecutorResponse, error) {
	body, headers, endpoint, err := e.freeCall(ctx, req, entry)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	var mapped []byte
	var mapErr error
	switch strings.ToLower(strings.TrimSpace(entry.Endpoint)) {
	case "responses":
		mapped, mapErr = assembleResponsesCompletion(req.Model, body)
	case "systemone":
		mapped, mapErr = mapToCompletion(req.Model, body)
	default:
		mapped, mapErr = assembleChatCompletion(req.Model, body)
	}
	if mapErr != nil {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadGateway, msg: mapErr.Error()}
	}
	if isEmptyCompletion(mapped) && e.cfg.Free.Quota.FailoverPaid && strings.TrimSpace(e.cfg.Free.Quota.PaidFallback) != "" {
		fbBody, fbHeaders, _, fbErr := e.paidFallback(ctx, req, nil)
		if fbErr != nil {
			return pluginapi.ExecutorResponse{}, fbErr
		}
		return pluginapi.ExecutorResponse{Payload: fbBody, Headers: fbHeaders}, nil
	}
	_ = endpoint
	return pluginapi.ExecutorResponse{Payload: mapped, Headers: headers}, nil
}

// isEmptyCompletion reports whether an assembled OpenAI completion carries
// no text and no tool calls (free-tier silent degradation). It never fails:
// unparseable bodies are treated as non-empty and surface normally.
func isEmptyCompletion(mapped []byte) bool {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(mapped, &decoded); err != nil {
		return false
	}
	if len(decoded.Choices) == 0 {
		return false
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content) == "" &&
		len(decoded.Choices[0].Message.ToolCalls) == 0
}

// executeFreeStream runs the free path and converts upstream SSE to OpenAI
// deltas (chat upstream already speaks OpenAI SSE and passes through).
func (e *Executor) executeFreeStream(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry) (pluginapi.ExecutorStreamResponse, error) {
	body, headers, _, err := e.freeCall(ctx, req, entry)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	kind := strings.ToLower(strings.TrimSpace(entry.Endpoint))
	if kind == "" || kind == "chat" {
		return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: forwardChatSSE(ctx, body, framingForRequest(req))}, nil
	}
	if kind == "systemone" {
		// Single-shot upstream: emit answers as one chunk.
		chunk, err := mapToStreamChunk(req.Model, body)
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
	return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: convertResponsesSSE(ctx, body, framingForRequest(req))}, nil
}

// freeCall performs member×session failover and returns one successful
// upstream response body. Each member already couples one identity with one
// egress, mirroring the opencode provider entries.
func (e *Executor) freeCall(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry) ([]byte, http.Header, string, error) {
	cooldown := e.cfg.cooldown()
	var lastErr error
	sessions := e.freeSessions(entry, req)
	order := e.freepool.order()
	attempts := 0
	for _, mi := range order {
		m := e.freepool.members[mi]
		for _, s := range sessions {
			if attempts >= 8 {
				break
			}
			attempts++
			status, headers, respBody, err := e.freeAttempt(ctx, req, entry, s, m, mi)
			if err != nil {
				lastErr = err
				if freeRetryable(0, err) && ctx.Err() == nil {
					e.coolFree(s, mi, cooldown)
					continue
				}
				return nil, nil, "", err
			}
			if status < 200 || status >= 300 {
				lastErr = statusError{statusCode: status, body: respBody}
				if freeRetryable(status, nil) && ctx.Err() == nil {
					e.coolFree(s, mi, cooldown)
					continue
				}
				return nil, nil, "", lastErr
			}
			e.sessions.report(s, true, 0)
			return respBody, headers, strings.ToLower(strings.TrimSpace(entry.Endpoint)), nil
		}
		if attempts >= 8 {
			break
		}
	}
	if e.cfg.Free.Quota.FailoverPaid && strings.TrimSpace(e.cfg.Free.Quota.PaidFallback) != "" {
		return e.paidFallback(ctx, req, lastErr)
	}
	if lastErr != nil {
		return nil, nil, "", lastErr
	}
	return nil, nil, "", statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: no healthy session or member"}
}

// freeSessions returns candidate session IDs: the interceptor-stamped
// sticky assignment when healthy, else round-robin picks (empty for
// systemone, which needs none). At most 3 sessions per request.
func (e *Executor) freeSessions(entry FreeModelEntry, req pluginapi.ExecutorRequest) []string {
	if stamped := strings.TrimSpace(req.Headers.Get(poolSessionHeader)); stamped != "" {
		if e.sessionUsable(stamped) {
			return []string{stamped}
		}
	}
	if strings.EqualFold(strings.TrimSpace(entry.Endpoint), "systemone") {
		return []string{""}
	}
	out := []string{}
	seen := map[string]struct{}{}
	for len(out) < 3 {
		s := e.sessions.pick()
		if s == "" {
			break
		}
		if _, dup := seen[s]; dup {
			break
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

func (e *Executor) sessionUsable(id string) bool {
	e.sessions.mu.Lock()
	defer e.sessions.mu.Unlock()
	now := time.Now().Unix()
	for i := range e.sessions.sessions {
		if e.sessions.sessions[i].ID != id {
			continue
		}
		return e.sessions.sessions[i].Fails < 5 && e.sessions.sessions[i].CooldownUntil <= now
	}
	return false
}

func (e *Executor) coolFree(session string, memberIdx int, cooldown int) {
	if session != "" {
		e.sessions.report(session, false, cooldown)
	}
	if memberIdx >= 0 {
		e.freepool.cool(memberIdx, cooldown)
	}
}

// paidFallback re-dispatches as a paid chat request for the configured
// fallback model (upstream chat model name, used verbatim).
func (e *Executor) paidFallback(ctx context.Context, req pluginapi.ExecutorRequest, freeErr error) ([]byte, http.Header, string, error) {
	fallback := strings.TrimSpace(e.cfg.Free.Quota.PaidFallback)
	members := e.cfg.members(req)
	if len(members) == 0 {
		if freeErr != nil {
			return nil, nil, "", freeErr
		}
		return nil, nil, "", statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	var decoded map[string]any
	if err := json.Unmarshal(requestPayload(req), &decoded); err != nil {
		return nil, nil, "", statusError{statusCode: http.StatusBadRequest, msg: "zen free executor: invalid chat payload"}
	}
	decoded["model"] = fallback
	body, err := json.Marshal(decoded)
	if err != nil {
		return nil, nil, "", statusError{statusCode: http.StatusBadGateway, msg: err.Error()}
	}
	url := strings.TrimSuffix(e.cfg.baseURL(), "/") + "/v1/chat/completions"
	var lastErr error = freeErr
	for _, idx := range e.keypool.order(members) {
		m := members[idx]
		d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
		if err != nil {
			lastErr = err
			continue
		}
		status, headers, respBody, err := d.do(ctx, url, upstreamHeaders(strings.TrimSpace(m.Key)), body)
		if err != nil {
			lastErr = err
			if retryable(0, err) && ctx.Err() == nil {
				continue
			}
			return nil, nil, "", err
		}
		if status < 200 || status >= 300 {
			lastErr = statusError{statusCode: status, body: respBody}
			if retryable(status, nil) && ctx.Err() == nil {
				continue
			}
			return nil, nil, "", lastErr
		}
		return respBody, headers, "chat", nil
	}
	if lastErr != nil {
		return nil, nil, "", lastErr
	}
	return nil, nil, "", statusError{statusCode: http.StatusBadGateway, msg: "zen paid fallback: all pool members failed"}
}

// freeRetryable reports whether a free-tier failure is worth rotating to
// the next member/session. Unlike the paid pool, HTTP 403 here is the
// normal gate/quota signal (FreeTierError), not a malformed request, so it
// must rotate instead of failing fast.
func freeRetryable(status int, err error) bool {
	if err != nil {
		return true
	}
	if status == 400 || status == 404 || status == 422 {
		return false
	}
	return true
}
func forwardChatSSE(ctx context.Context, sse []byte, framing streamFraming) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk, 8)
	go func() {
		defer close(out)
		emit := func(payload []byte) bool {
			if len(bytes.TrimSpace(payload)) == 0 {
				return true
			}
			if framing == framingClaude {
				payload = append([]byte("data: "), payload...)
			}
			select {
			case <-ctx.Done():
				return false
			case out <- pluginapi.ExecutorStreamChunk{Payload: payload}:
				return true
			}
		}
		for _, line := range bytes.Split(sse, []byte("\n")) {
			trimmed := bytes.TrimSpace(line)
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if len(payload) == 0 || string(payload) == "[DONE]" {
				continue
			}
			if !emit(payload) {
				return
			}
		}
	}()
	return out
}

// convertResponsesSSE converts Responses SSE to OpenAI deltas incrementally.
// A terminal empty chunk is emitted when nothing else was, so downstream
// never sees a bare stream end as a transport error.
func convertResponsesSSE(ctx context.Context, sse []byte, framing streamFraming) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk, 8)
	go func() {
		defer close(out)
		c := newResponsesConverter()
		emitted := 0
		emit := func(payload []byte) bool {
			if len(bytes.TrimSpace(payload)) == 0 {
				return true
			}
			if framing == framingClaude {
				payload = append([]byte("data: "), payload...)
			}
			select {
			case <-ctx.Done():
				return false
			case out <- pluginapi.ExecutorStreamChunk{Payload: payload}:
				emitted++
				return true
			}
		}
		for _, line := range bytes.Split(sse, []byte("\n")) {
			if out := c.convertLine(line); len(bytes.TrimSpace(out)) > 0 {
				if !emit(out) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		if emitted == 0 {
			terminal, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			emit(terminal)
		}
	}()
	return out
}
