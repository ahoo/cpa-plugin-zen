package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	mrand "math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	path     string
}

// burnedSessionCooldown is the penalty for a session the upstream rejects
// with 403 FreeTierError: it is dead, not flaky, so park it for hours
// instead of minutes.
const burnedSessionCooldown = 6 * 3600

// sessionCoverage caps how many pool sessions one request may try. The pool
// is mostly corpses during upstream crunches; 3 was too few to reach a
// live one.
const sessionCoverage = 6

func loadSessionPool(path string) *sessionPool {
	p := &sessionPool{path: strings.TrimSpace(path)}
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
		p.saveLocked()
		return
	}
}

// saveLocked persists health counters so restarts don't resurrect burned
// sessions. Caller must hold p.mu. Best-effort: a failed write only means
// the next restart retries a corpse, which rotation already handles.
func (p *sessionPool) saveLocked() {
	if strings.TrimSpace(p.path) == "" {
		return
	}
	raw, err := json.Marshal(struct {
		Sessions []sessionEntry `json:"sessions"`
	}{Sessions: p.sessions})
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p.path)
}

// poolSessionHeader carries the sticky pool assignment from the
// interceptor to the executor, and echoes the serving session back on
// responses. Internal only: executors build their own upstream headers
// and never forward it.
const poolSessionHeader = "X-Zen-Pool-Session"

// fallbackHeader marks responses served by paid fallback instead of the free
// tier, so e2e can tell "free down, fallback covered" apart from "free served".
const fallbackHeader = "X-Zen-Fallback"

// stampPoolSession records the pool session that served the request on the
// response headers for observability (e2e stickiness assertions). It never
// touches upstream-bound headers: executors build those separately.
func stampPoolSession(headers http.Header, session string) http.Header {
	if strings.TrimSpace(session) == "" {
		return headers
	}
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set(poolSessionHeader, session)
	return headers
}

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
	c := &http.Client{Transport: transport, Timeout: 20 * time.Second}
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
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
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
			if len(m.ToolCalls) != 0 {
				return nil, fmt.Errorf("zen free executor: tool calls on instruction message")
			}
			if instructions == "" {
				instructions = text
			} else {
				instructions += "\n" + text
			}
		case "user", "assistant":
			if text != "" || len(m.ToolCalls) == 0 {
				input = append(input, map[string]any{"role": m.Role, "content": text})
			}
			for _, call := range m.ToolCalls {
				if call.ID == "" || (call.Type != "" && call.Type != "function") || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
					return nil, fmt.Errorf("zen free executor: invalid assistant tool call")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		case "tool":
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("zen free executor: tool result missing tool_call_id")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": text})
		default:
			return nil, fmt.Errorf("zen free executor: unsupported message role %q", m.Role)
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

// sseEvents joins all data fields belonging to one SSE event. Line-by-line
// JSON parsing breaks valid multiline events and can discard upstream errors.
func sseEvents(body []byte) ([][]byte, bool, error) {
	var events [][]byte
	var fields [][]byte
	done := false
	flush := func() error {
		if len(fields) == 0 {
			return nil
		}
		payload := bytes.TrimSpace(bytes.Join(fields, []byte("\n")))
		fields = nil
		for bytes.HasPrefix(payload, []byte("data:")) {
			payload = bytes.TrimSpace(payload[len("data:"):])
		}
		if done {
			return statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: data after SSE termination"}
		}
		if string(payload) == "[DONE]" {
			done = true
			return nil
		}
		if !json.Valid(payload) {
			return statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: malformed upstream SSE event"}
		}
		events = append(events, payload)
		return nil
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if err := flush(); err != nil {
				return nil, false, err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			fields = append(fields, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	if len(fields) != 0 {
		// A final newline is a valid last event even without an extra blank
		// line; an unterminated data line may be a partial HTTP read.
		if !bytes.HasSuffix(body, []byte("\n")) {
			return nil, false, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: truncated upstream SSE event"}
		}
		if err := flush(); err != nil {
			return nil, false, err
		}
	}
	return events, done, nil
}

func upstreamSSEError(payload []byte) error {
	var ev struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: invalid upstream event"}
	}
	if ev.Type == "error" || ev.Type == "response.error" || ev.Type == "response.failed" || ev.Type == "response.incomplete" || ev.Type == "response.cancelled" || len(ev.Error) != 0 && string(ev.Error) != "null" {
		msg := upstreamErrorMessage(payload)
		if msg == "" || msg == string(payload) {
			msg = "zen free executor: upstream stream failed"
		}
		return statusError{statusCode: http.StatusBadGateway, msg: msg}
	}
	return nil
}

// assembleChatCompletion folds OpenAI SSE events into one completion.
type collectedTool struct {
	id, name, arguments string
}

func collectedCalls(tools map[int]*collectedTool) []any {
	indices := make([]int, 0, len(tools))
	for idx := range tools {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	calls := make([]any, 0, len(indices))
	for _, idx := range indices {
		tool := tools[idx]
		calls = append(calls, map[string]any{
			"id": tool.id, "type": "function",
			"function": map[string]any{"name": tool.name, "arguments": tool.arguments},
		})
	}
	return calls
}

func assembleChatCompletion(requestedModel string, sse []byte) ([]byte, error) {
	events, done, err := sseEvents(sse)
	if err != nil {
		return nil, err
	}
	var content strings.Builder
	finish := ""
	tools := map[int]*collectedTool{}
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	for _, payload := range events {
		if err := upstreamSSEError(payload); err != nil {
			return nil, err
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
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
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: invalid chat SSE chunk"}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content.WriteString(choice.Delta.Content)
			for _, call := range choice.Delta.ToolCalls {
				tool := tools[call.Index]
				if tool == nil {
					tool = &collectedTool{}
					tools[call.Index] = tool
				}
				if call.ID != "" {
					tool.id = call.ID
				}
				if call.Function.Name != "" {
					tool.name = call.Function.Name
				}
				tool.arguments += call.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			usage["prompt_tokens"] = chunk.Usage.PromptTokens
			usage["completion_tokens"] = chunk.Usage.CompletionTokens
			usage["total_tokens"] = chunk.Usage.TotalTokens
		}
	}
	if !done || finish == "" {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: incomplete chat stream"}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if len(tools) != 0 {
		for _, tool := range tools {
			if tool.id == "" || tool.name == "" || !json.Valid([]byte(tool.arguments)) {
				return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: incomplete chat tool call"}
			}
		}
		message["tool_calls"] = collectedCalls(tools)
	}
	return json.Marshal(map[string]any{
		"id": completionID(), "object": "chat.completion", "created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   usage,
	})
}

// responsesConverter turns Responses SSE into OpenAI deltas incrementally.
type responsesConverter struct {
	indexByItem map[string]int
	callIDs     map[string]string
	arguments   map[string]string
	textByPart  map[string]string
	nextIndex   int
	toolCalls   bool
	content     bool
	completed   bool
	usage       map[string]any
	err         error
}

func newResponsesConverter() *responsesConverter {
	return &responsesConverter{
		indexByItem: map[string]int{}, callIDs: map[string]string{}, arguments: map[string]string{}, textByPart: map[string]string{},
		usage: map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

func (c *responsesConverter) toolIndex(id string) int {
	if idx, ok := c.indexByItem[id]; ok {
		return idx
	}
	idx := c.nextIndex
	c.nextIndex++
	c.indexByItem[id] = idx
	c.toolCalls = true
	return idx
}

func (c *responsesConverter) convertLine(line []byte) []byte {
	payload := bytes.TrimSpace(line)
	if !bytes.HasPrefix(payload, []byte("data:")) {
		return nil
	}
	for bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return nil
	}
	return c.convertEvent(payload)
}

func (c *responsesConverter) convertEvent(payload []byte) []byte {
	if c.err != nil {
		return nil
	}
	if err := upstreamSSEError(payload); err != nil {
		c.err = err
		return nil
	}
	var ev struct {
		Type      string `json:"type"`
		Delta     string `json:"delta"`
		Text      string `json:"text"`
		Arguments string `json:"arguments"`
		Item      *struct {
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		ContentIndex int    `json:"content_index"`
		Response     *struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
			Usage  *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: invalid Responses SSE event"}
		return nil
	}
	if c.completed {
		c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: event after Responses completion"}
		return nil
	}
	part := fmt.Sprintf("%s/%d/%d", ev.ItemID, ev.OutputIndex, ev.ContentIndex)
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil
		}
		c.textByPart[part] += ev.Delta
		c.content = true
		return marshalDelta(map[string]any{"role": "assistant", "content": ev.Delta}, "", nil)
	case "response.output_text.done":
		if ev.Text == "" {
			return nil
		}
		seen := c.textByPart[part]
		if !strings.HasPrefix(ev.Text, seen) {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: inconsistent Responses text"}
			return nil
		}
		rest := strings.TrimPrefix(ev.Text, seen)
		c.textByPart[part] = ev.Text
		if rest == "" {
			return nil
		}
		c.content = true
		return marshalDelta(map[string]any{"role": "assistant", "content": rest}, "", nil)
	case "response.output_item.added", "response.output_item.done":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return nil
		}
		if ev.Item.ID == "" || ev.Item.CallID == "" || ev.Item.Name == "" {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: incomplete Responses tool item"}
			return nil
		}
		idx, exists := c.indexByItem[ev.Item.ID]
		if !exists {
			idx = c.toolIndex(ev.Item.ID)
			c.callIDs[ev.Item.ID] = ev.Item.CallID
		} else if c.callIDs[ev.Item.ID] != ev.Item.CallID {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: inconsistent Responses call id"}
			return nil
		}
		var calls []any
		if !exists {
			calls = append(calls, map[string]any{"index": idx, "id": ev.Item.CallID, "type": "function", "function": map[string]any{"name": ev.Item.Name, "arguments": ""}})
		}
		if ev.Item.Arguments != "" {
			seen := c.arguments[ev.Item.ID]
			if !strings.HasPrefix(ev.Item.Arguments, seen) {
				c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: inconsistent Responses tool arguments"}
				return nil
			}
			if suffix := strings.TrimPrefix(ev.Item.Arguments, seen); suffix != "" {
				calls = append(calls, map[string]any{"index": idx, "function": map[string]any{"arguments": suffix}})
			}
			c.arguments[ev.Item.ID] = ev.Item.Arguments
		}
		if len(calls) == 0 {
			return nil
		}
		return marshalDelta(nil, "", calls)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		if ev.ItemID == "" {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: tool arguments missing item id"}
			return nil
		}
		idx, ok := c.indexByItem[ev.ItemID]
		if !ok {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: tool arguments before tool item"}
			return nil
		}
		arg := ev.Delta
		if ev.Type == "response.function_call_arguments.done" {
			if !strings.HasPrefix(ev.Arguments, c.arguments[ev.ItemID]) {
				c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: inconsistent tool arguments"}
				return nil
			}
			arg = strings.TrimPrefix(ev.Arguments, c.arguments[ev.ItemID])
		}
		if arg == "" {
			return nil
		}
		c.arguments[ev.ItemID] += arg
		return marshalDelta(nil, "", []any{map[string]any{"index": idx, "function": map[string]any{"arguments": arg}}})
	case "response.completed":
		if c.completed || ev.Response == nil || ev.Response.Status != "completed" {
			c.err = statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: Responses did not complete successfully"}
			return nil
		}
		c.completed = true
		if ev.Response.Usage != nil {
			c.usage["prompt_tokens"] = ev.Response.Usage.InputTokens
			c.usage["completion_tokens"] = ev.Response.Usage.OutputTokens
			c.usage["total_tokens"] = ev.Response.Usage.InputTokens + ev.Response.Usage.OutputTokens
		}
		finish := "stop"
		if c.toolCalls {
			finish = "tool_calls"
		}
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": c.usage})
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
	events, _, err := sseEvents(sse)
	if err != nil {
		return nil, err
	}
	c := newResponsesConverter()
	var content strings.Builder
	toolArgs := map[int]string{}
	toolNames := map[int]string{}
	toolIDs := map[int]string{}
	finish := ""
	usage := c.usage
	for _, payload := range events {
		out := c.convertEvent(payload)
		if c.err != nil {
			return nil, c.err
		}
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
			return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: invalid converted Responses chunk"}
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
	if !c.completed || finish == "" {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: incomplete Responses stream"}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if len(c.indexByItem) > 0 {
		tools := make(map[int]*collectedTool, len(c.indexByItem))
		for id, idx := range c.indexByItem {
			if toolIDs[idx] == "" || toolNames[idx] == "" || !json.Valid([]byte(toolArgs[idx])) {
				return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: incomplete Responses tool call"}
			}
			tools[idx] = &collectedTool{id: c.callIDs[id], name: toolNames[idx], arguments: toolArgs[idx]}
		}
		message["tool_calls"] = collectedCalls(tools)
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
		return 0, nil, nil, statusError{statusCode: http.StatusBadRequest, msg: err.Error()}
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
	return d.do(ctx, http.MethodPost, url, headers, body)
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

func fallbackEligible(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var upstream statusError
	if errors.As(err, &upstream) && upstream.statusCode >= 400 && upstream.statusCode < 500 {
		return upstream.statusCode == http.StatusUnauthorized || upstream.statusCode == http.StatusForbidden || upstream.statusCode == http.StatusTooManyRequests
	}
	return true
}

func freeCompletion(model, endpoint string, body []byte) ([]byte, error) {
	var mapped []byte
	var err error
	switch endpoint {
	case "responses":
		mapped, err = assembleResponsesCompletion(model, body)
	case "systemone":
		mapped, err = mapToCompletion(model, body)
	default:
		mapped, err = assembleChatCompletion(model, body)
	}
	if err != nil {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: err.Error()}
	}
	if isEmptyCompletion(mapped) {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "zen free executor: upstream returned no content or tools"}
	}
	return mapped, nil
}

// executeFree makes at most one paid-fallback decision for a free request.
func (e *Executor) executeFree(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry) (pluginapi.ExecutorResponse, error) {
	body, headers, endpoint, err := e.freeCall(ctx, req, entry)
	if err == nil {
		body, err = freeCompletion(req.Model, endpoint, body)
	}
	if err == nil {
		return pluginapi.ExecutorResponse{Payload: body, Headers: headers}, nil
	}
	if !e.cfg.Free.Quota.FailoverPaid || strings.TrimSpace(e.cfg.Free.Quota.PaidFallback) == "" || !fallbackEligible(ctx, err) {
		return pluginapi.ExecutorResponse{}, err
	}
	paid, paidHeaders, _, fbErr := e.paidFallback(ctx, req, err)
	if fbErr != nil {
		return pluginapi.ExecutorResponse{}, fbErr
	}
	if validateCompletion(paid) != nil {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "zen paid fallback: invalid or empty completion"}
	}
	if paidHeaders == nil {
		paidHeaders = http.Header{}
	}
	paidHeaders.Set(fallbackHeader, "paid")
	return pluginapi.ExecutorResponse{Payload: paid, Headers: paidHeaders}, nil
}

// validateCompletion rejects malformed paid JSON before it becomes a successful
// stream (or is returned to a non-streaming caller) with invented termination.
func validateCompletion(body []byte) error {
	var completion struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) == 0 {
		return fmt.Errorf("invalid chat completion")
	}
	choice := completion.Choices[0]
	if choice.FinishReason == "" || strings.TrimSpace(choice.Message.Content) == "" && len(choice.Message.ToolCalls) == 0 {
		return fmt.Errorf("empty or unfinished chat completion")
	}
	for _, call := range choice.Message.ToolCalls {
		if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return fmt.Errorf("invalid chat completion tool call")
		}
	}
	return nil
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

// executeFreeStream validates the already-buffered upstream response before
// emitting any successful chunk, so errors and empty replies can fall back
// without racing a previously sent finish_reason.
func (e *Executor) executeFreeStream(ctx context.Context, req pluginapi.ExecutorRequest, entry FreeModelEntry) (pluginapi.ExecutorStreamResponse, error) {
	body, headers, endpoint, err := e.freeCall(ctx, req, entry)
	var mapped []byte
	if err == nil {
		mapped, err = freeCompletion(req.Model, endpoint, body)
	}
	framing := framingForRequest(req)
	if err != nil {
		if !e.cfg.Free.Quota.FailoverPaid || strings.TrimSpace(e.cfg.Free.Quota.PaidFallback) == "" || !fallbackEligible(ctx, err) {
			return pluginapi.ExecutorStreamResponse{}, err
		}
		paid, paidHeaders, _, fbErr := e.paidFallback(ctx, req, err)
		if fbErr != nil {
			return pluginapi.ExecutorStreamResponse{}, fbErr
		}
		pieces := chunkCompletion(req.Model, paid, framing)
		if len(pieces) == 0 {
			return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "zen paid fallback: invalid or empty completion"}
		}
		if paidHeaders == nil {
			paidHeaders = http.Header{}
		}
		paidHeaders.Set(fallbackHeader, "paid")
		return pluginapi.ExecutorStreamResponse{Headers: paidHeaders, Chunks: completionChunks(ctx, pieces)}, nil
	}
	if ctx.Err() != nil {
		return pluginapi.ExecutorStreamResponse{}, ctx.Err()
	}
	if endpoint == "systemone" {
		return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: completionChunks(ctx, chunkCompletion(req.Model, mapped, framing))}, nil
	}
	if endpoint == "responses" {
		return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: convertResponsesSSE(ctx, body, framing)}, nil
	}
	return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: forwardChatSSE(ctx, body, framing)}, nil
}

func completionChunks(ctx context.Context, pieces [][]byte) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk, len(pieces))
	go func() {
		defer close(out)
		for _, piece := range pieces {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- pluginapi.ExecutorStreamChunk{Payload: piece}:
			}
		}
	}()
	return out
}

// chunkCompletion splits a non-stream OpenAI completion into stream deltas.
func chunkCompletion(model string, fbBody []byte, framing streamFraming) [][]byte {
	if validateCompletion(fbBody) != nil {
		return nil
	}
	var decoded struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(fbBody, &decoded) != nil {
		return nil
	}
	choice := decoded.Choices[0]
	if decoded.ID == "" {
		decoded.ID = completionID()
	}
	if decoded.Created == 0 {
		decoded.Created = time.Now().Unix()
	}
	var pieces [][]byte
	enc := func(choice map[string]any, usage json.RawMessage) {
		chunk := map[string]any{
			"id": decoded.ID, "object": "chat.completion.chunk", "created": decoded.Created,
			"model": model, "choices": []any{choice},
		}
		if len(usage) != 0 {
			chunk["usage"] = usage
		}
		raw, _ := json.Marshal(chunk)
		if framing == framingClaude {
			raw = append([]byte("data: "), raw...)
		}
		pieces = append(pieces, raw)
	}
	content := choice.Message.Content
	for i := 0; i < len(content); {
		end := i + 500
		if end > len(content) {
			end = len(content)
		}
		for end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		if end == i {
			_, size := utf8.DecodeRuneInString(content[i:])
			end += size
		}
		enc(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content[i:end]}}, nil)
		i = end
	}
	if len(choice.Message.ToolCalls) != 0 {
		calls := make([]any, 0, len(choice.Message.ToolCalls))
		for idx, call := range choice.Message.ToolCalls {
			var obj map[string]any
			if json.Unmarshal(call, &obj) != nil {
				return nil
			}
			obj["index"] = idx
			calls = append(calls, obj)
		}
		enc(map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}}, nil)
	}
	enc(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": choice.FinishReason}, decoded.Usage)
	return pieces
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
	consecutive := 0
	for _, mi := range order {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		// cool() mutates health on concurrent requests; copy the identity
		// under the same lock before executing network I/O.
		e.freepool.mu.Lock()
		m := e.freepool.members[mi]
		e.freepool.mu.Unlock()
		for _, s := range sessions {
			if err := ctx.Err(); err != nil {
				return nil, nil, "", err
			}
			if attempts >= 12 {
				break
			}
			// Three straight failures means the free tier is down, not
			// flaky: stop burning timeouts and fall through to paid.
			if consecutive >= 3 {
				break
			}
			attempts++
			status, headers, respBody, err := e.freeAttempt(ctx, req, entry, s, m, mi)
			if err != nil {
				lastErr = err
				if fallbackEligible(ctx, err) && ctx.Err() == nil {
					e.coolFree(s, mi, cooldown)
					consecutive++
					continue
				}
				return nil, nil, "", err
			}
			if status < 200 || status >= 300 {
				lastErr = statusError{statusCode: status, body: respBody}
				if (freeRetryable(status, nil) || freeBodyRetryable(respBody)) && ctx.Err() == nil {
					if status == http.StatusForbidden {
						// 403 FreeTierError means this session is burned:
						// park it for hours and keep rotating. It must NOT
						// count toward the consecutive breaker, which is
						// for transport-level outages.
						e.sessions.report(s, false, burnedSessionCooldown)
						e.freepool.cool(mi, cooldown)
						continue
					}
					e.coolFree(s, mi, cooldown)
					consecutive++
					continue
				}
				return nil, nil, "", lastErr
			}
			if err := ctx.Err(); err != nil {
				return nil, nil, "", err
			}
			e.sessions.report(s, true, 0)
			stampPoolSession(headers, s)
			return respBody, headers, strings.ToLower(strings.TrimSpace(entry.Endpoint)), nil
		}
		if attempts >= 12 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
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
	for len(out) < sessionCoverage {
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
	decoded["stream"] = false
	delete(decoded, "stream_options")
	if floor := float64(e.cfg.fallbackFloor()); floor > 0 {
		if mt, ok := decoded["max_tokens"].(float64); !ok || mt < floor {
			decoded["max_tokens"] = floor
		}
	}
	body, err := json.Marshal(decoded)
	if err != nil {
		return nil, nil, "", statusError{statusCode: http.StatusBadGateway, msg: err.Error()}
	}
	url := strings.TrimSuffix(e.cfg.baseURL(), "/") + "/v1/chat/completions"
	var lastErr error = freeErr
	for _, idx := range e.keypool.order(members) {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		m := members[idx]
		d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
		if err != nil {
			lastErr = err
			continue
		}
		status, headers, respBody, err := d.do(ctx, http.MethodPost, url, upstreamHeaders(strings.TrimSpace(m.Key)), body)
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
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
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
// must rotate instead of failing fast. Bodies hinting at transient upstream
// state (unavailable/overloaded/...) rotate regardless of status.
func freeRetryable(status int, err error) bool {
	if err != nil {
		return true
	}
	if status == 400 || status == 404 || status == 422 {
		return false
	}
	return true
}

// freeBodyRetryable sniffs response bodies for transient upstream signals
// that arrive under non-retryable statuses (e.g. endpoint unavailable).
func freeBodyRetryable(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))
	if len(lowered) > 2000 {
		lowered = lowered[:2000]
	}
	for _, hint := range []string{
		"unavailable", "overloaded", "try again", "capacity",
		"temporarily", "timeout", "timed out", "busy", "overloaded_error",
		"rate limit", "rate_limit", "ratelimit", "too many requests",
	} {
		if strings.Contains(lowered, hint) {
			return true
		}
	}
	return false
}
func forwardChatSSE(ctx context.Context, sse []byte, framing streamFraming) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk, 8)
	go func() {
		defer close(out)
		emit := func(chunk pluginapi.ExecutorStreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- chunk:
				return true
			}
		}
		mapped, err := assembleChatCompletion("", sse)
		if err != nil {
			emit(pluginapi.ExecutorStreamChunk{Err: err})
			return
		}
		if isEmptyCompletion(mapped) {
			return
		}
		events, _, err := sseEvents(sse)
		if err != nil {
			emit(pluginapi.ExecutorStreamChunk{Err: err})
			return
		}
		for _, payload := range events {
			if framing == framingClaude {
				payload = append([]byte("data: "), payload...)
			}
			if !emit(pluginapi.ExecutorStreamChunk{Payload: payload}) {
				return
			}
		}
	}()
	return out
}

// convertResponsesSSE converts validated Responses SSE to OpenAI deltas.
// Empty completed responses emit no success chunks; executeFreeStream checks
// for emptiness before selecting the single paid fallback.
func convertResponsesSSE(ctx context.Context, sse []byte, framing streamFraming) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk, 8)
	go func() {
		defer close(out)
		emit := func(chunk pluginapi.ExecutorStreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- chunk:
				return true
			}
		}
		mapped, err := assembleResponsesCompletion("", sse)
		if err != nil {
			emit(pluginapi.ExecutorStreamChunk{Err: err})
			return
		}
		if isEmptyCompletion(mapped) {
			return
		}
		events, _, err := sseEvents(sse)
		if err != nil {
			emit(pluginapi.ExecutorStreamChunk{Err: err})
			return
		}
		c := newResponsesConverter()
		for _, payload := range events {
			chunk := c.convertEvent(payload)
			if c.err != nil {
				emit(pluginapi.ExecutorStreamChunk{Err: c.err})
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if framing == framingClaude {
				chunk = append([]byte("data: "), chunk...)
			}
			if !emit(pluginapi.ExecutorStreamChunk{Payload: chunk}) {
				return
			}
		}
	}()
	return out
}
