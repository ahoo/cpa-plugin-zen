package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Synthetic SSE only: no post-[DONE] response bytes have been captured.
func mimoTailTestEvent(payload string) string {
	return "data: " + payload + "\n\n"
}

func mimoTailCheck(t *testing.T, body string, want mimoTailSummary) {
	t.Helper()
	raw := summarizeMimoSSETail([]byte(body))
	if len(raw) > 1024 {
		t.Fatalf("diagnostic exceeds 1024 bytes: %d", len(raw))
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields) != 14 {
		t.Fatalf("expected fixed-size JSON object; fields=%d, error=%v", len(fields), err)
	}
	for _, value := range fields {
		switch value.(type) {
		case bool, float64:
		default:
			t.Fatalf("unexpected non-numeric or non-boolean summary value: %T", value)
		}
	}
	var got mimoTailSummary
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got != want {
		t.Fatalf("summary mismatch: got %+v, want %+v, error=%v", got, want, err)
	}
}

func TestMimoTailSummaryCostPingIsOpaque(t *testing.T) {
	for _, tc := range []struct{ name, cost string }{
		{"number", `12.5`},
		{"string", `"cost-token-SENTINEL"`},
		{"boolean", `true`},
		{"object", `{"output":"SENTINEL","error":{"message":"SENTINEL"}}`},
		{"array", `[1,null,"SENTINEL"]`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mimoTailTestEvent("[DONE]") + mimoTailTestEvent(fmt.Sprintf(`{"type":"ping","cost":%s}`, tc.cost))
			mimoTailCheck(t, body, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1})
			if strings.Contains(summarizeMimoSSETail([]byte(body)), "SENTINEL") {
				t.Fatal("opaque cost leaked into diagnostic")
			}
		})
	}
	mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(`{"type":"ping","cost":null}`), mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1})
	mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(fmt.Sprintf(`{"type":"p%cu0069ng","cost":null}`, '\\')), mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1,
	})
}

func TestMimoTailSummaryLateErrorAndExtraFields(t *testing.T) {
	body := mimoTailTestEvent("[DONE]") + mimoTailTestEvent(`{"type":"ping","cost":null}`) +
		mimoTailTestEvent(`{"type":"error","error":{"message":"SENTINEL_error_from_upstream"}}`) +
		mimoTailTestEvent(`{"type":"response.failed","response":{"status":"failed","error":{"message":"SENTINEL_nested_error"}}}`)
	mimoTailCheck(t, body, mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 3, StrictCostPingCount: 1,
		ExtraFieldsCount: 2, OutputFieldsCount: 1, ErrorFieldsCount: 2, OtherTypeCount: 2,
	})
	raw := summarizeMimoSSETail([]byte(body))
	if strings.Contains(raw, "SENTINEL") || strings.Contains(raw, "response.failed") {
		t.Fatal("upstream error message or event type leaked")
	}

	body = mimoTailTestEvent("[DONE]") + mimoTailTestEvent(`{"type":"ping","cost":null,"private-SENTINEL":"SENTINEL","output":null,"response":{},"choices":[],"usage":{},"error":null}`)
	mimoTailCheck(t, body, mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 1, ExtraFieldsCount: 6,
		OutputFieldsCount: 3, UsageFieldsCount: 1, ErrorFieldsCount: 1,
	})
	if strings.Contains(summarizeMimoSSETail([]byte(body)), "SENTINEL") {
		t.Fatal("unknown key or value leaked")
	}
	mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(`{"type":"ping"}`), mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 1,
	})
	mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(`{"type":"telemetry","cost":0}`), mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 1, OtherTypeCount: 1,
	})
}

func TestMimoTailSummaryInvalidAndDuplicateJSON(t *testing.T) {
	mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(`{"type":"ping","cost":`)+
		mimoTailTestEvent(`["SENTINEL_NON_OBJECT"]`)+mimoTailTestEvent(`{"type":"ping","cost":null}`), mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 3, InvalidJSONCount: 1, NonObjectCount: 1, StrictCostPingCount: 1,
	})
	for _, tc := range []struct {
		name, payload string
		want          mimoTailSummary
	}{
		{"early-type-7-overwritten", `{"type":7,"type":"ping","cost":null}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1, TypedInvalidCount: 1, OtherTypeCount: 1}},
		{"null-type", `{"type":null,"cost":null}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, TypedInvalidCount: 1, OtherTypeCount: 1}},
		{"duplicate-cost", `{"type":"ping","cost":1,"cost":null}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1}},
		{"escaped-duplicate-cost", fmt.Sprintf(`{"type":"ping","c%cu006fst":1,"cost":null}`, '\\'), mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1}},
		{"duplicate-with-output", `{"type":"ping","cost":1,"cost":null,"output":null}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1, ExtraFieldsCount: 1, OutputFieldsCount: 1}},
		{"typed-invalid-delta-overwritten", `{"type":"ping","cost":null,"delta":7,"delta":"late"}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1, TypedInvalidCount: 1, ExtraFieldsCount: 2, OutputFieldsCount: 2}},
		{"typed-invalid-response-overwritten", `{"type":"ping","cost":null,"response":7,"response":{}}`, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, DuplicateTopKeysCount: 1, TypedInvalidCount: 1, ExtraFieldsCount: 2, OutputFieldsCount: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mimoTailCheck(t, mimoTailTestEvent("[DONE]")+mimoTailTestEvent(tc.payload), tc.want)
		})
	}
}

func TestMimoTailSummaryScansAllPostDoneFrames(t *testing.T) {
	body := mimoTailTestEvent("[DONE]") + mimoTailTestEvent(`{"type":"ping","cost":null}`) +
		mimoTailTestEvent(`{"type":"ping","cost":{"secret":"SENTINEL"}}`) + mimoTailTestEvent("[DONE]") +
		mimoTailTestEvent(`{"type":"telemetry","cost":5}`) + mimoTailTestEvent(`{"type":"ping","cost":false}`)
	mimoTailCheck(t, body, mimoTailSummary{
		Complete: true, DoneCount: 2, TailFrames: 5, StrictCostPingCount: 3,
		DuplicateDoneCount: 1, OtherTypeCount: 1,
	})
	body = mimoTailTestEvent(`{"type":"ping","cost":null}`) + mimoTailTestEvent(`{"type":"unrelated"}`)
	mimoTailCheck(t, body, mimoTailSummary{}) // No first framed [DONE].
}

func TestMimoTailSummaryCRLFMultilineCommentsAndNestedData(t *testing.T) {
	body := ": before\r\n" + "data: data: [DONE]\r\n\r\n" + ": keepalive after DONE\r\n" +
		"event: ping\r\n" + "data: data: {\"type\":\r\n" + "data: \"ping\",\"cost\":null}\r\n\r\n" +
		": last comment without newline"
	mimoTailCheck(t, body, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1})
	body = mimoTailTestEvent(`{"type":"malformed_prefix":`) + mimoTailTestEvent("[DONE]") + mimoTailTestEvent(`{"type":"ping","cost":null}`)
	mimoTailCheck(t, body, mimoTailSummary{Complete: true, DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1})
}

func TestMimoTailSummaryLimitsAndIncompleteSSE(t *testing.T) {
	ping := mimoTailTestEvent(`{"type":"ping","cost":null}`)
	done := mimoTailTestEvent("[DONE]")
	mimoTailCheck(t, done+strings.Repeat(ping, 32)+": keepalive\n", mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 32, StrictCostPingCount: 32,
	})
	mimoTailCheck(t, done+strings.Repeat(ping, 33), mimoTailSummary{
		DoneCount: 1, TailFrames: 32, StrictCostPingCount: 32,
	})
	// Framing and comments consume the 64 KiB budget, not only JSON payloads.
	mimoTailCheck(t, done+": "+strings.Repeat("z", mimoTailByteLimit)+"\n"+ping, mimoTailSummary{DoneCount: 1})
	mimoTailCheck(t, done+`data: {"type":"ping","cost":null}`, mimoTailSummary{
		DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1,
	})
	mimoTailCheck(t, `data: [DONE]`, mimoTailSummary{})
	mimoTailCheck(t, done+": keepalive without final newline", mimoTailSummary{Complete: true, DoneCount: 1})
	// Cap the search as well as the tail, and never report complete on excess.
	mimoTailCheck(t, ": "+strings.Repeat("z", mimoTailFullScanLimit)+"\n"+done+ping, mimoTailSummary{})
	prefix := ": " + strings.Repeat("p", mimoTailFullScanLimit-100) + "\n"
	mimoTailCheck(t, prefix+done+ping+strings.Repeat(": after\n", 40), mimoTailSummary{
		DoneCount: 1, TailFrames: 1, StrictCostPingCount: 1,
	})
}

func TestMimoTailSummaryNeverLeaksRawStrings(t *testing.T) {
	const sentinel = "SENTINEL_PRIVATE_TOKEN_9ab038f0"
	body := mimoTailTestEvent("[DONE]") + mimoTailTestEvent(`{"type":"`+sentinel+`","cost":{"secret":"`+sentinel+`"},"`+sentinel+`":"`+sentinel+`","text":"`+sentinel+`"}`) +
		mimoTailTestEvent(`{"`+sentinel+`":`) + ": " + sentinel + "\n"
	raw := summarizeMimoSSETail([]byte(body))
	if !json.Valid([]byte(raw)) || strings.Contains(raw, sentinel) || strings.Contains(raw, "PRIVATE") || len(raw) > 1024 {
		t.Fatal("diagnostic leaked untrusted SSE bytes, was invalid JSON, or exceeded its bound")
	}
	mimoTailCheck(t, body, mimoTailSummary{
		Complete: true, DoneCount: 1, TailFrames: 2, InvalidJSONCount: 1,
		ExtraFieldsCount: 2, OutputFieldsCount: 1, OtherTypeCount: 1,
	})
}
