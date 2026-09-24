package plugin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// All post-[DONE] events below are synthetic; none is a captured fixture.
func mimoDetailEvent(data string) string { return "data: " + data + "\n\n" }

func mimoDetailCheck(t *testing.T, body string) (mimoTailDetailEnvelope, string) {
	t.Helper()
	raw := summarizeMimoSSETailDetails([]byte(body))
	if len(mimoTailDetailMarker)+len(raw) > 1024 {
		t.Fatalf("marker plus detailed diagnostic exceeds 1024 bytes: %d", len(mimoTailDetailMarker)+len(raw))
	}
	if mimoTailDetailMarker != "; tail_structure_v2=" {
		t.Fatal("unexpected detail marker")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil || len(top) != 4 {
		t.Fatalf("detailed envelope must have four fixed JSON keys: %v", err)
	}
	for _, key := range []string{"schema", "summary", "detail_complete", "frames"} {
		if _, exists := top[key]; !exists {
			t.Fatalf("missing fixed envelope key %s", key)
		}
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(top["summary"], &summary); err != nil || len(summary) != 14 {
		t.Fatalf("legacy summary schema changed: %v", err)
	}
	var frames []map[string]json.RawMessage
	if err := json.Unmarshal(top["frames"], &frames); err != nil || frames == nil {
		t.Fatalf("frames must be a non-null array: %v", err)
	}
	for _, frame := range frames {
		if len(frame) != 3 || frame["kind"] == nil || frame["fields"] == nil || frame["unknown_fields"] == nil {
			t.Fatal("frame schema must contain exactly kind, fields, unknown_fields")
		}
		var pairs [][]string
		if err := json.Unmarshal(frame["fields"], &pairs); err != nil || pairs == nil {
			t.Fatalf("fields must be a non-null array of pairs: %v", err)
		}
		for _, pair := range pairs {
			if len(pair) != 2 {
				t.Fatal("field entry must be a two-string array")
			}
		}
	}
	var got mimoTailDetailEnvelope
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got.Schema != 2 {
		t.Fatalf("invalid detailed schema: %v, schema=%d", err, got.Schema)
	}
	var legacy mimoTailSummary
	if err := json.Unmarshal([]byte(summarizeMimoSSETail([]byte(body))), &legacy); err != nil || got.Summary != legacy {
		t.Fatalf("detailed summary differs from legacy: %v, detailed=%+v, legacy=%+v", err, got.Summary, legacy)
	}
	return got, raw
}

func TestMimoTailDetailExactEnvelopeFramesAndFraming(t *testing.T) {
	body := ": comment before\r\n" + "data: data: [DONE]\r\n\r\n" + ": keepalive\r\n" +
		"data: data: {\"type\":\r\ndata: \"ping\",\"cost\":null}\r\n\r\n" +
		mimoDetailEvent("[DONE]") + mimoDetailEvent(`["secret"]`) + mimoDetailEvent(`{"type":"ping","cost":`) + ": comment after"
	got, _ := mimoDetailCheck(t, body)
	if !got.DetailComplete || !got.Summary.Complete || got.Summary.DoneCount != 2 || len(got.Frames) != 4 {
		t.Fatalf("missing framed events: %+v", got)
	}
	want := []mimoTailDetailFrame{
		{Kind: "object", Fields: [][2]string{{"type", "ping"}, {"cost", "opaque"}}},
		{Kind: "done", Fields: [][2]string{}},
		{Kind: "non_object", Fields: [][2]string{}},
		{Kind: "invalid_json", Fields: [][2]string{}},
	}
	if !reflect.DeepEqual(got.Frames, want) {
		t.Fatalf("frame order, kind or pairs changed: got %+v, want %+v", got.Frames, want)
	}
}

func TestMimoTailDetailKnownShapesAndWhitelist(t *testing.T) {
	for _, tc := range []struct{ name, value, shape string }{
		{"null", `null`, "null"},
		{"empty-array", `[  ]`, "empty_array"},
		{"nonempty-array", `["PRIVATE_ARRAY"]`, "nonempty_array"},
		{"empty-object", `{ }`, "empty_object"},
		{"nonempty-object", `{"private":"PRIVATE_OBJECT"}`, "nonempty_object"},
		{"empty-string", `""`, "empty_string"},
		{"nonempty-string", `"PRIVATE_STRING"`, "nonempty_string"},
		{"number", `-12.5e2`, "number"},
		{"boolean-true", `true`, "boolean"},
		{"boolean-false", `false`, "boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mimoDetailEvent("[DONE]") + mimoDetailEvent(fmt.Sprintf(`{"type":"ping","cost":%s,"output":%s}`, tc.value, tc.value))
			got, raw := mimoDetailCheck(t, body)
			want := [][2]string{{"type", "ping"}, {"cost", "opaque"}, {"output", tc.shape}}
			if !got.DetailComplete || len(got.Frames) != 1 || !reflect.DeepEqual(got.Frames[0].Fields, want) {
				t.Fatalf("value shape or opaque cost changed: %+v", got.Frames)
			}
			for _, secret := range []string{"PRIVATE_ARRAY", "PRIVATE_OBJECT", "PRIVATE_STRING", "-12.5e2"} {
				if strings.Contains(raw, secret) {
					t.Fatal("upstream cost/output value leaked")
				}
			}
		})
	}
	for _, key := range []string{
		"output", "response", "choices", "delta", "text", "arguments", "item", "item_id", "content", "message",
		"tool_calls", "function", "function_call", "finish_reason", "output_index", "content_index", "usage", "error",
	} {
		t.Run("field-"+key, func(t *testing.T) {
			body := mimoDetailEvent("[DONE]") + mimoDetailEvent(fmt.Sprintf(`{"type":"ping","cost":null,%q:null}`, key))
			got, _ := mimoDetailCheck(t, body)
			want := [][2]string{{"type", "ping"}, {"cost", "opaque"}, {key, "null"}}
			if len(got.Frames) != 1 || got.Frames[0].UnknownFields != 0 || !reflect.DeepEqual(got.Frames[0].Fields, want) {
				t.Fatalf("whitelisted field missing, reordered or unrecognized: %+v", got.Frames)
			}
		})
	}
}

func TestMimoTailDetailTypeEnumsDuplicatesAndUnknowns(t *testing.T) {
	for _, kind := range []string{"error", "response.error", "response.failed", "response.incomplete", "response.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			got, _ := mimoDetailCheck(t, mimoDetailEvent("[DONE]")+mimoDetailEvent(fmt.Sprintf(`{"type":%q}`, kind)))
			if len(got.Frames) != 1 || !reflect.DeepEqual(got.Frames[0].Fields, [][2]string{{"type", "known_error"}}) {
				t.Fatalf("known error type was not flagged: %+v", got.Frames)
			}
		})
	}
	const secret = "SENTINEL_PRIVATE_TOOL_ARGUMENTS_URL_ID_87651"
	body := mimoDetailEvent("[DONE]") + mimoDetailEvent(`{"type":7,"type":"ping","cost":{"private":"`+secret+`"},"type":"`+secret+`","id":"`+secret+`","`+secret+`":"`+secret+`","output":[]}`)
	got, raw := mimoDetailCheck(t, body)
	want := [][2]string{{"type", "invalid"}, {"type", "ping"}, {"cost", "opaque"}, {"type", "other_string"}, {"output", "empty_array"}}
	if len(got.Frames) != 1 || !reflect.DeepEqual(got.Frames[0].Fields, want) || got.Frames[0].UnknownFields != 2 ||
		got.Summary.DuplicateTopKeysCount != 2 || got.Summary.TypedInvalidCount != 1 || got.Summary.StrictCostPingCount != 0 {
		t.Fatalf("early invalid and duplicate type hidden: %+v", got)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, `"id"`) {
		t.Fatal("unknown key, event type, id or opaque cost leaked")
	}
	// Escaped known names are recognized, but both original occurrences remain.
	body = mimoDetailEvent("[DONE]") + mimoDetailEvent(fmt.Sprintf(`{"type":"p%cu0069ng","c%cu006fst":null,"cost":true,"ou%cu0074put":false}`, '\\', '\\', '\\'))
	got, _ = mimoDetailCheck(t, body)
	want = [][2]string{{"type", "ping"}, {"cost", "opaque"}, {"cost", "opaque"}, {"output", "boolean"}}
	if len(got.Frames) != 1 || !reflect.DeepEqual(got.Frames[0].Fields, want) || got.Frames[0].UnknownFields != 0 || got.Summary.DuplicateTopKeysCount != 1 {
		t.Fatalf("escaped known names lost their decoded order or duplicates: %+v", got)
	}
	got, _ = mimoDetailCheck(t, mimoDetailEvent("[DONE]")+mimoDetailEvent(`{"cost":null,"private":"private"}`))
	if !reflect.DeepEqual(got.Frames[0].Fields, [][2]string{{"cost", "opaque"}}) || got.Frames[0].UnknownFields != 1 {
		t.Fatal("absent type must be omitted, not invented")
	}
}

func TestMimoTailDetailOrderedFramesBeyondAggregates(t *testing.T) {
	ping := mimoDetailEvent(`{"cost":null,"type":"ping","output":[]}`)
	errorFrame := mimoDetailEvent(`{"type":"error","error":{"message":"PRIVATE"}}`)
	first, firstRaw := mimoDetailCheck(t, mimoDetailEvent("[DONE]")+ping+errorFrame)
	second, secondRaw := mimoDetailCheck(t, mimoDetailEvent("[DONE]")+errorFrame+ping)
	if first.Summary != second.Summary || !first.DetailComplete || !second.DetailComplete || firstRaw == secondRaw {
		t.Fatal("identical aggregates must still distinguish different frame order")
	}
	if !reflect.DeepEqual(first.Frames[0].Fields, [][2]string{{"cost", "opaque"}, {"type", "ping"}, {"output", "empty_array"}}) ||
		!reflect.DeepEqual(second.Frames[0].Fields, [][2]string{{"type", "known_error"}, {"error", "nonempty_object"}}) {
		t.Fatal("per-frame original key order or error flags were lost")
	}
	if strings.Contains(firstRaw, "PRIVATE") || strings.Contains(secondRaw, "PRIVATE") {
		t.Fatal("late error body leaked")
	}
}

func TestMimoTailDetailBoundsAndNoPartialFrameLists(t *testing.T) {
	done := mimoDetailEvent("[DONE]")
	ping := mimoDetailEvent(`{"type":"ping","cost":null}`)
	got, _ := mimoDetailCheck(t, done+ping)
	if !got.DetailComplete || len(got.Frames) != 1 {
		t.Fatal("small complete tail must retain its frame")
	}
	got, _ = mimoDetailCheck(t, done+strings.Repeat(ping, 20))
	if !got.Summary.Complete || got.Summary.TailFrames != 20 || got.DetailComplete || len(got.Frames) != 0 {
		t.Fatal("oversized detail must drop ALL frames without changing complete legacy summary")
	}
	got, _ = mimoDetailCheck(t, done+strings.Repeat(ping, 33))
	if got.Summary.Complete || got.Summary.TailFrames != 32 || got.DetailComplete || len(got.Frames) != 0 {
		t.Fatal("frame limit must not report partial detailed tail")
	}
	got, _ = mimoDetailCheck(t, done+`data: {"type":"ping","cost":null}`)
	if got.Summary.Complete || got.Summary.StrictCostPingCount != 1 || got.DetailComplete || len(got.Frames) != 0 {
		t.Fatal("unterminated data must not expose a supposedly complete frame list")
	}
	got, _ = mimoDetailCheck(t, done+": "+strings.Repeat("z", mimoTailByteLimit)+"\n"+ping)
	if got.Summary.Complete || got.DetailComplete || len(got.Frames) != 0 {
		t.Fatal("64 KiB tail including comments must be bounded")
	}
	got, _ = mimoDetailCheck(t, ": "+strings.Repeat("z", mimoTailFullScanLimit)+"\n"+done+ping)
	if got.Summary.DoneCount != 0 || got.Summary.Complete || got.DetailComplete || len(got.Frames) != 0 {
		t.Fatal("1 MiB prefix must be bounded")
	}
}
