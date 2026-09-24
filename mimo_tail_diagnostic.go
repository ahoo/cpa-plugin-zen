package plugin

import (
	"bytes"
	"encoding/json"
)

const (
	mimoTailFullScanLimit = 1 << 20  // Bound the search for the first framed [DONE].
	mimoTailByteLimit     = 64 << 10 // Includes SSE framing and comments after [DONE].
	mimoTailFrameLimit    = 32
	mimoTailDetailMarker  = "; tail_structure_v2="
)

// mimoTailSummary contains only fixed keys, booleans and bounded counters.
// complete means that the scanner covered the entire SSE suffix after the
// first [DONE], not that any response (even an all-ping tail) is acceptable.
type mimoTailSummary struct {
	Complete              bool `json:"complete"`
	DoneCount             int  `json:"done_count"`
	TailFrames            int  `json:"tail_frames"`
	StrictCostPingCount   int  `json:"strict_cost_ping_count"`
	DuplicateDoneCount    int  `json:"duplicate_done_count"`
	InvalidJSONCount      int  `json:"invalid_json_count"`
	NonObjectCount        int  `json:"non_object_count"`
	DuplicateTopKeysCount int  `json:"duplicate_top_keys_count"`
	TypedInvalidCount     int  `json:"typed_invalid_count"`
	ExtraFieldsCount      int  `json:"extra_fields_count"`
	OutputFieldsCount     int  `json:"output_fields_count"`
	UsageFieldsCount      int  `json:"usage_fields_count"`
	ErrorFieldsCount      int  `json:"error_fields_count"`
	OtherTypeCount        int  `json:"other_type_count"`
}

// A detail frame contains only schema-controlled literals and counters. Fields
// are ordered pairs, not a map: an earlier invalid/duplicate key stays visible.
type mimoTailDetailFrame struct {
	Kind          string      `json:"kind"`
	Fields        [][2]string `json:"fields"`
	UnknownFields int         `json:"unknown_fields"`
}

type mimoTailDetailEnvelope struct {
	Schema         int                   `json:"schema"`
	Summary        mimoTailSummary       `json:"summary"`
	DetailComplete bool                  `json:"detail_complete"`
	Frames         []mimoTailDetailFrame `json:"frames"`
}

// summarizeMimoSSETail is diagnostic only. It does not change SSE parsing or
// authorize any post-termination data. Never include payloads, keys, event
// types, IDs, cost values or malformed input in the returned JSON.
func summarizeMimoSSETail(body []byte) string {
	result, _ := json.Marshal(mimoTailAnalyze(body, nil)) // A fixed struct cannot fail.
	return string(result)
}

// summarizeMimoSSETailDetails exposes each scanned frame's structural shape,
// never upstream strings. It neither changes nor authorizes parser behavior.
// The caller appends mimoTailDetailMarker; the combined suffix fits 1024 bytes.
func summarizeMimoSSETailDetails(body []byte) string {
	frames := make([]mimoTailDetailFrame, 0)
	summary := mimoTailAnalyze(body, &frames)
	envelope := mimoTailDetailEnvelope{Schema: 2, Summary: summary, DetailComplete: summary.Complete, Frames: frames}
	if !summary.Complete {
		// Never imply that a partial scan describes all the trailing frames.
		envelope.Frames = []mimoTailDetailFrame{}
	}
	result, _ := json.Marshal(envelope)
	if len(mimoTailDetailMarker)+len(result) > 1024 {
		envelope.DetailComplete = false
		envelope.Frames = []mimoTailDetailFrame{}
		result, _ = json.Marshal(envelope)
	}
	return string(result)
}

func mimoTailAnalyze(body []byte, frames *[]mimoTailDetailFrame) mimoTailSummary {
	var summary mimoTailSummary
	prefixEnd := min(len(body), mimoTailFullScanLimit)
	start, found := mimoTailFirstDone(body[:prefixEnd])
	if found {
		summary.DoneCount = 1
		tailEnd := min(len(body), prefixEnd, start+mimoTailByteLimit)
		summary.Complete = tailEnd == len(body)
		mimoTailScan(body[start:tailEnd], &summary, frames)
	}
	return summary
}

// mimoTailPayload follows sseEvents: join all data lines in an SSE frame,
// trim whitespace, then peel any nested data: prefixes from the joined body.
func mimoTailPayload(fields [][]byte) []byte {
	payload := bytes.TrimSpace(bytes.Join(fields, []byte("\n")))
	for bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	return payload
}

func mimoTailFirstDone(body []byte) (int, bool) {
	var fields [][]byte
	for pos := 0; pos < len(body); {
		lineEnd := bytes.IndexByte(body[pos:], '\n')
		end := len(body)
		if lineEnd >= 0 {
			end = pos + lineEnd
		}
		line := bytes.TrimSuffix(body[pos:end], []byte("\r"))
		next := end
		if lineEnd >= 0 {
			next++
		}
		if len(line) == 0 {
			if len(fields) != 0 && bytes.Equal(mimoTailPayload(fields), []byte("[DONE]")) {
				return next, true
			}
			fields = fields[:0]
		} else if bytes.HasPrefix(line, []byte("data:")) {
			field := line[len("data:"):]
			fields = append(fields, bytes.TrimPrefix(field, []byte(" ")))
		}
		pos = next
	}
	// A final newline terminates a data line even without another blank line.
	// A final un-terminated data line does not constitute a complete [DONE].
	if len(fields) != 0 && bytes.HasSuffix(body, []byte("\n")) && bytes.Equal(mimoTailPayload(fields), []byte("[DONE]")) {
		return len(body), true
	}
	return 0, false
}

func mimoTailScan(tail []byte, summary *mimoTailSummary, frames *[]mimoTailDetailFrame) {
	var fields [][]byte
	flush := func() bool {
		if len(fields) == 0 {
			return true
		}
		if summary.TailFrames == mimoTailFrameLimit {
			summary.Complete = false
			return false
		}
		payload := mimoTailPayload(fields)
		fields = fields[:0]
		summary.TailFrames++
		if bytes.Equal(payload, []byte("[DONE]")) {
			summary.DoneCount++
			summary.DuplicateDoneCount++
			if frames != nil {
				*frames = append(*frames, mimoTailDetailFrame{Kind: "done", Fields: [][2]string{}})
			}
		} else if frames != nil {
			frame := mimoTailDetailFrame{Fields: [][2]string{}}
			mimoTailInspectPayload(payload, summary, &frame)
			*frames = append(*frames, frame)
		} else {
			mimoTailInspectPayload(payload, summary, nil)
		}
		return true
	}
	for pos := 0; pos < len(tail); {
		lineEnd := bytes.IndexByte(tail[pos:], '\n')
		end := len(tail)
		if lineEnd >= 0 {
			end = pos + lineEnd
		}
		line := bytes.TrimSuffix(tail[pos:end], []byte("\r"))
		if len(line) == 0 {
			if !flush() {
				return
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			field := line[len("data:"):]
			fields = append(fields, bytes.TrimPrefix(field, []byte(" ")))
		}
		pos = end
		if lineEnd >= 0 {
			pos++
		}
	}
	if len(fields) != 0 {
		if !bytes.HasSuffix(tail, []byte("\n")) {
			// Inspect the partial frame too, but never claim a complete scan.
			summary.Complete = false
		}
		flush()
	}
}

// mimoTailInspectPayload operates on the original JSON bytes, not a map that
// would erase duplicate keys and early type errors before validation.
func mimoTailInspectPayload(payload []byte, summary *mimoTailSummary, frame *mimoTailDetailFrame) {
	if !json.Valid(payload) {
		summary.InvalidJSONCount++
		if frame != nil {
			frame.Kind = "invalid_json"
		}
		return
	}
	if len(payload) == 0 || payload[0] != '{' {
		summary.NonObjectCount++
		if frame != nil {
			frame.Kind = "non_object"
		}
		return
	}
	if frame != nil {
		frame.Kind = "object"
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	_, _ = dec.Token() // '{' (json.Valid already checked the original bytes).
	seen := make(map[string]struct{})
	var typeSeen, pingType, otherType, costSeen, errorField, errorType bool
	var duplicates, invalidTyped int
	for dec.More() {
		keyToken, _ := dec.Token()
		key := keyToken.(string)
		var raw json.RawMessage
		_ = dec.Decode(&raw)
		if _, present := seen[key]; present {
			duplicates++
			summary.DuplicateTopKeysCount++
		}
		seen[key] = struct{}{}
		if !mimoTailTypedFieldValid(key, raw) {
			invalidTyped++
			summary.TypedInvalidCount++
		}
		typeShape := "invalid"
		switch key {
		case "type":
			typeSeen = true
			if len(raw) > 0 && raw[0] == '"' {
				var kind string
				_ = json.Unmarshal(raw, &kind)
				if kind == "ping" {
					pingType = true
					typeShape = "ping"
				} else {
					otherType = true
					typeShape = "other_string"
					if mimoTailErrorType(kind) {
						errorType = true
						typeShape = "known_error"
					}
				}
			} else {
				otherType = true
			}
		case "cost":
			costSeen = true // Any valid JSON value, including null, is opaque.
		default:
			summary.ExtraFieldsCount++
			switch key {
			case "output", "response", "choices", "delta", "text", "arguments", "item", "item_id", "content", "message", "tool_calls", "function", "function_call", "finish_reason", "output_index", "content_index":
				summary.OutputFieldsCount++
			case "usage", "input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens":
				summary.UsageFieldsCount++
			case "error":
				errorField = true
				summary.ErrorFieldsCount++
			}
		}
		if frame != nil {
			switch key {
			case "type":
				frame.Fields = append(frame.Fields, [2]string{"type", typeShape})
			case "cost":
				frame.Fields = append(frame.Fields, [2]string{"cost", "opaque"})
			default:
				if mimoTailDetailFieldKnown(key) {
					frame.Fields = append(frame.Fields, [2]string{key, mimoTailValueShape(raw)})
				} else {
					frame.UnknownFields++
				}
			}
		}
	}
	_, _ = dec.Token() // '}'
	if !typeSeen || otherType {
		summary.OtherTypeCount++
	}
	if pingType && costSeen && len(seen) == 2 && duplicates == 0 && invalidTyped == 0 {
		summary.StrictCostPingCount++
	}
	if errorType && !errorField {
		// A known failure event without an error field is still explicit.
		summary.ErrorFieldsCount++
	}
}

// Only these fixed field names can appear in the detail. Unknown names are
// counted, never serialized; decoded escaped spellings still match the list.
func mimoTailDetailFieldKnown(key string) bool {
	switch key {
	case "output", "response", "choices", "delta", "text", "arguments", "item", "item_id", "content", "message", "tool_calls", "function", "function_call", "finish_reason", "output_index", "content_index", "usage", "error":
		return true
	default:
		return false
	}
}

func mimoTailValueShape(raw []byte) string {
	switch raw[0] { // All callers have validated the entire JSON frame.
	case 'n':
		return "null"
	case '[':
		if len(bytes.TrimSpace(raw[1:len(raw)-1])) == 0 {
			return "empty_array"
		}
		return "nonempty_array"
	case '{':
		if len(bytes.TrimSpace(raw[1:len(raw)-1])) == 0 {
			return "empty_object"
		}
		return "nonempty_object"
	case '"':
		if len(raw) == 2 {
			return "empty_string"
		}
		return "nonempty_string"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}

func mimoTailErrorType(kind string) bool {
	switch kind {
	case "error", "response.error", "response.failed", "response.incomplete", "response.cancelled":
		return true
	default:
		return false
	}
}

// Validate each original occurrence, so an invalid first type followed by a
// valid duplicate cannot masquerade as a strict ping. cost/error are opaque.
func mimoTailTypedFieldValid(key string, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	switch key {
	case "type", "delta", "text", "arguments", "item_id", "status":
		return raw[0] == '"'
	case "response", "item", "usage":
		return raw[0] == '{' || bytes.Equal(raw, []byte("null"))
	case "choices", "output", "tool_calls":
		return raw[0] == '[' || bytes.Equal(raw, []byte("null"))
	case "output_index", "content_index":
		var n int
		return json.Unmarshal(raw, &n) == nil && raw[0] != 'n'
	default:
		return true
	}
}
