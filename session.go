package plugin

import (
	"strings"
	"unicode"
)

// session.go forwards client conversation session ids to OpenCode Zen as
// x-opencode-session for sticky routing and prompt-cache affinity.
//
// Ported from cpa-plugin-opencode-session-mapper (standalone interceptor):
// same sources, same fail-closed validation, same metadata fallback. It runs
// as request_interceptor before and after credential selection.

// sessionSources lists downstream client headers that carry a stable
// per-conversation session id, in priority order.
var sessionSources = []string{
	"Session-Id",                    // codex CLI
	"Session_id",                    // codex CLI underscore variant
	"Thread-Id",                     // codex CLI thread = conversation
	"Thread_id",                     // underscore variant
	"X-Claude-Code-Session-Id",      // claude code
	"X-DeepSeek-Harness-Session-Id", // dsh / deepseek-harness native provider
	"X-Session-Affinity",            // dsh pi-ai (anthropic-messages, openai-completions)
	"X-Session-Id",                  // generic (opencode-ish clients)
	"X-Client-Request-Id",           // last resort (dsh pi-ai openai-responses)
}

const (
	maxSessionIDBytes = 1024

	targetSessionHeader = "X-Opencode-Session"
	targetClientHeader  = "X-Opencode-Client"
	defaultClientValue  = "cliproxy"
)

// normalizeSessionID trims and validates one session identifier.
func normalizeSessionID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSessionIDBytes {
		return "", false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", false
		}
	}
	return value, true
}

// sessionHeaderValue returns a normalized session identifier and whether the
// header name was present. Empty values, invalid values, or conflicting
// repeated / case-variant values are reported as present with an empty value
// so callers fail closed instead of selecting a value using randomized map
// order.
func sessionHeaderValue(headers map[string][]string, name string) (string, bool) {
	matched := false
	selected := ""
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		matched = true
		if len(values) == 0 {
			return "", true
		}
		for _, raw := range values {
			value, valid := normalizeSessionID(raw)
			if !valid {
				return "", true
			}
			if selected == "" {
				selected = value
				continue
			}
			if selected != value {
				return "", true
			}
		}
	}
	if !matched {
		return "", false
	}
	return selected, true
}

func sessionHeaderHasValue(headers map[string][]string, name string) bool {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

// mapSessionHeaders implements the interception: client session headers (or
// the host-computed fallback) become X-Opencode-Session / X-Opencode-Client.
// It returns the headers to inject (nil/empty when nothing applies).
func mapSessionHeaders(headers map[string][]string, metadata map[string]any) map[string][]string {
	if headers == nil {
		return nil
	}
	// Client already sent x-opencode-session (e.g. real opencode CLI):
	// never override, their value is authoritative.
	if _, exists := sessionHeaderValue(headers, targetSessionHeader); exists {
		return nil
	}
	var session string
	for _, src := range sessionSources {
		if v, exists := sessionHeaderValue(headers, src); exists {
			if v == "" {
				return nil
			}
			session = v
			break
		}
	}
	if session == "" {
		session = sessionFallback(metadata)
	}
	if session == "" {
		return nil
	}
	out := map[string][]string{targetSessionHeader: {session}}
	// Identify the proxy only when the client didn't identify itself.
	if !sessionHeaderHasValue(headers, targetClientHeader) {
		out[targetClientHeader] = []string{defaultClientValue}
	}
	return out
}

// sessionFallback returns the host-computed conversation identity for
// clients that send no session header of their own.
//
// canonical_session_id is built by the core from the source protocol and the
// client's own session id. It is populated only on the after-auth pass, which
// is why this is a fallback and not a source. derived_session_id and
// lcp_affinity_session_id are deliberately NOT used: the former is absent
// from this payload on current hosts, and core documents the latter as
// unusable for provider conversation identity.
func sessionFallback(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	v, ok := metadata["canonical_session_id"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	value, valid := normalizeSessionID(s)
	if !valid {
		return ""
	}
	return value
}
