package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const responsesTailCompleted = `data: {"type":"response.completed","response":{"status":"completed","error":null,"usage":{"input_tokens":4,"output_tokens":7}}}`

// Synthetic SSE only: the diagnostic retained event structure, not upstream bytes or cost values.
func responsesTailBody(beforeCompleted, afterCompleted []string, done bool) []byte {
	lines := []string{
		`data: {"type":"response.output_text.delta","item_id":"txt","delta":"Hello "}`,
		`data: {"type":"response.output_text.done","item_id":"txt","text":"Hello Zen"}`,
		`data: {"type":"response.output_item.added","item":{"id":"fc","call_id":"call","type":"function_call","name":"read"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc","delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc","arguments":"{\"path\":\"a\"}"}`,
		`data: {"type":"response.output_item.done","item":{"id":"fc","call_id":"call","type":"function_call","name":"read","arguments":"{\"path\":\"a\"}"}}`,
	}
	for _, event := range beforeCompleted {
		lines = append(lines, "data: "+event)
	}
	lines = append(lines, responsesTailCompleted)
	for _, event := range afterCompleted {
		lines = append(lines, "data: "+event)
	}
	if done {
		lines = append(lines, "data: [DONE]")
	}
	return freeFixSSE(lines...)
}

func responsesTailFixture(t *testing.T, body []byte, fallback bool) (*Executor, pluginapi.ExecutorRequest, *freeFixFakeHTTP) {
	t.Helper()
	path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
	e := availabilityExecutor(path, fallback)
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(request.URL, "/v1/responses"):
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
		case fallback && strings.HasSuffix(request.URL, "/v1/chat/completions"):
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"Paid"},"finish_reason":"stop"}]}`)}, nil
		default:
			t.Fatal("Responses tail dispatched an unexpected upstream request")
			return pluginapi.HTTPResponse{}, nil
		}
	}}
	return e, freeFixRequest(fake), fake
}

func responsesTailSingleFreeCall(t *testing.T, fake *freeFixFakeHTTP) {
	t.Helper()
	calls := fake.snapshot()
	if len(calls) != 1 || !strings.HasSuffix(calls[0].URL, "/v1/responses") {
		t.Fatalf("expected one fake Responses call and no paid fallback, got %d calls", len(calls))
	}
}

func responsesTailSuccess(t *testing.T, e *Executor, req pluginapi.ExecutorRequest, stream bool) {
	t.Helper()
	if !stream {
		resp, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatalf("valid accounting tail rejected: %v", err)
		}
		if resp.Headers.Get(fallbackHeader) != "" {
			t.Fatal("valid free response incorrectly used paid fallback")
		}
		var got struct {
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
			Usage map[string]int64 `json:"usage"`
		}
		if err := json.Unmarshal(resp.Payload, &got); err != nil {
			t.Fatalf("invalid completion JSON: %v", err)
		}
		if len(got.Choices) != 1 || got.Choices[0].FinishReason != "tool_calls" || got.Choices[0].Message.Content != "Hello Zen" || got.Usage["prompt_tokens"] != 4 || got.Usage["completion_tokens"] != 7 || got.Usage["total_tokens"] != 11 {
			t.Fatal("accounting ping duplicated or lost text, completion or usage")
		}
		tools := got.Choices[0].Message.ToolCalls
		if len(tools) != 1 || tools[0].ID != "call" || tools[0].Function.Name != "read" || tools[0].Function.Arguments != `{"path":"a"}` {
			t.Fatal("accounting ping duplicated or lost the tool call")
		}
		return
	}

	resp, err := e.ExecuteStream(context.Background(), req)
	if err != nil {
		t.Fatalf("valid accounting stream rejected: %v", err)
	}
	if resp.Headers.Get(fallbackHeader) != "" {
		t.Fatal("valid free stream incorrectly used paid fallback")
	}
	var text, finish, toolID, toolName, arguments string
	var finishes, usages int
	var tokens map[string]int64
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			t.Fatalf("valid stream emitted error: %v", chunk.Err)
		}
		parsed := freeFixChunk(t, chunk.Payload)
		for _, choice := range parsed.Choices {
			text += choice.Delta.Content
			for _, tool := range choice.Delta.ToolCalls {
				if tool.ID != "" {
					toolID = tool.ID
				}
				if tool.Function.Name != "" {
					toolName = tool.Function.Name
				}
				arguments += tool.Function.Arguments
			}
			if choice.FinishReason != nil {
				finishes++
				finish = *choice.FinishReason
			}
		}
		if parsed.Usage != nil {
			usages++
			tokens = parsed.Usage
		}
	}
	if text != "Hello Zen" || toolID != "call" || toolName != "read" || arguments != `{"path":"a"}` || finishes != 1 || finish != "tool_calls" || usages != 1 || tokens["prompt_tokens"] != 4 || tokens["completion_tokens"] != 7 || tokens["total_tokens"] != 11 {
		t.Fatal("ping must not emit extra terminal/usage frames or duplicate content/tools")
	}
}

func responsesTailRejected(t *testing.T, e *Executor, req pluginapi.ExecutorRequest, stream bool) error {
	t.Helper()
	if !stream {
		resp, err := e.Execute(context.Background(), req)
		if len(resp.Payload) != 0 {
			t.Fatal("invalid Responses tail emitted a nonstream completion")
		}
		availabilityStatus(t, err, http.StatusBadGateway)
		return err
	}
	resp, err := e.ExecuteStream(context.Background(), req)
	if resp.Chunks != nil {
		for chunk := range resp.Chunks {
			if chunk.Err == nil {
				t.Fatal("invalid Responses tail emitted a successful stream chunk")
			}
			if err == nil {
				err = chunk.Err
			}
		}
	}
	availabilityStatus(t, err, http.StatusBadGateway)
	return err
}

func TestResponsesTailCostIsOpaqueAndCompletionOccursOnce(t *testing.T) {
	for _, cost := range []struct {
		name, json string
	}{
		{"number", `12.5`},
		{"string", `"opaque"`},
		{"boolean", `true`},
		{"object", `{"unit":"opaque"}`},
		{"array", `[1,null]`},
		{"null", `null`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", cost.name, stream), func(t *testing.T) {
				body := responsesTailBody(nil, []string{fmt.Sprintf(`{"type":"ping","cost":%s}`, cost.json)}, false) // EOF without [DONE]
				e, req, fake := responsesTailFixture(t, body, false)
				responsesTailSuccess(t, e, req, stream)
				responsesTailSingleFreeCall(t, fake)
			})
		}
	}
	for _, tc := range []struct {
		name  string
		done  bool
		pings []string
	}{
		{"done-after-ping", true, []string{`{"type":"ping","cost":{"unit":"opaque"}}`}},
		{"escaped-type-done", true, []string{fmt.Sprintf(`{"type":"p%cu0069ng","cost":null}`, 92)}},
		{"repeated-ping-eof", false, []string{`{"type":"ping","cost":1}`, `{"type":"ping","cost":null}`}},
		{"repeated-ping-done", true, []string{`{"type":"ping","cost":null}`, `{"type":"ping","cost":[1,2]}`}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				e, req, fake := responsesTailFixture(t, responsesTailBody(nil, tc.pings, tc.done), false)
				responsesTailSuccess(t, e, req, stream)
				responsesTailSingleFreeCall(t, fake)
			})
		}
	}
}

func TestResponsesTailPreCompletionPingKeepsPriorHandling(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignored-before-valid-completion/stream=%t", stream), func(t *testing.T) {
			e, req, fake := responsesTailFixture(t, responsesTailBody([]string{`{"type":"ping","cost":null}`}, nil, false), false)
			responsesTailSuccess(t, e, req, stream)
			responsesTailSingleFreeCall(t, fake)
		})
		t.Run(fmt.Sprintf("ping-alone-incomplete/stream=%t", stream), func(t *testing.T) {
			e, req, fake := responsesTailFixture(t, freeFixSSE(`data: {"type":"ping","cost":null}`), false)
			responsesTailRejected(t, e, req, stream)
			responsesTailSingleFreeCall(t, fake)
		})
	}
}

func TestResponsesTailRejectsAnythingOtherThanPostCompletionCostPing(t *testing.T) {
	cases := []struct {
		name, tail, wantError string
	}{
		{"missing-cost", `{"type":"ping"}`, ""},
		{"uppercase-Type", `{"Type":"ping","cost":null}`, ""},
		{"uppercase-Cost", `{"type":"ping","Cost":null}`, ""},
		{"mixed-case-keys", `{"tYpe":"ping","coSt":null}`, ""},
		{"missing-cost-with-null-extra", `{"type":"ping","delta":null}`, ""},
		{"extra-delta", `{"type":"ping","cost":null,"delta":"late"}`, ""},
		{"extra-text", `{"type":"ping","cost":1,"text":"late"}`, ""},
		{"extra-response", `{"type":"ping","cost":{},"response":null}`, ""},
		{"extra-output", `{"type":"ping","cost":[],"output":null}`, ""},
		{"extra-error-null", `{"type":"ping","cost":false,"error":null}`, ""},
		{"extra-sequence", `{"type":"ping","cost":1,"sequence_number":3}`, ""},
		{"explicit-error-priority", `{"type":"ping","cost":null,"error":{"message":"upstream broke"}}`, "upstream broke"},
		{"unknown-type", `{"type":"telemetry","cost":0}`, ""},
		{"duplicate-completed", `{"type":"response.completed","response":{"status":"completed"}}`, ""},
		{"late-text-done", `{"type":"response.output_text.done","item_id":"txt","text":"Hello Zen later"}`, ""},
		{"late-new-content", `{"type":"response.output_text.delta","item_id":"txt","delta":"late"}`, ""},
		{"typed-invalid-first-type", `{"type":7,"type":"ping","cost":null}`, ""},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				e, req, fake := responsesTailFixture(t, responsesTailBody(nil, []string{tc.tail}, false), false)
				err := responsesTailRejected(t, e, req, stream)
				if tc.wantError != "" && !strings.Contains(err.Error(), tc.wantError) {
					t.Fatal("explicit upstream error must take precedence over generic tail error")
				}
				responsesTailSingleFreeCall(t, fake)
			})
		}
	}
}

func TestResponsesTailDoesNotSkipEventsAfterAccountingPing(t *testing.T) {
	for _, later := range []struct{ name, event string }{
		{"explicit-error", `{"type":"error","error":{"message":"upstream broke"}}`},
		{"response-failed", `{"type":"response.failed","response":{"status":"failed","error":{"message":"upstream broke"}}}`},
		{"duplicate-completed", `{"type":"response.completed","response":{"status":"completed"}}`},
		{"late-text-done", `{"type":"response.output_text.done","item_id":"txt","text":"Hello Zen later"}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", later.name, stream), func(t *testing.T) {
				body := responsesTailBody(nil, []string{`{"type":"ping","cost":null}`, later.event}, false)
				e, req, fake := responsesTailFixture(t, body, false)
				responsesTailRejected(t, e, req, stream)
				responsesTailSingleFreeCall(t, fake)
			})
		}
	}
}

func TestResponsesTailEarlyTypedInvalidFieldsCannotBeOverwritten(t *testing.T) {
	baseline := responsesTailBody(nil, []string{`{"type":"ping","cost":null}`}, false)
	cases := []struct {
		name, from, to string
	}{
		{"text-delta", `data: {"type":"response.output_text.delta","item_id":"txt","delta":"Hello "}`, `data: {"type":"response.output_text.delta","item_id":"txt","delta":7,"delta":"Hello "}`},
		{"completed-response", responsesTailCompleted, `data: {"type":"response.completed","response":7,"response":{"status":"completed","usage":{"input_tokens":4,"output_tokens":7}}}`},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := bytes.Replace(baseline, []byte(tc.from), []byte(tc.to), 1)
				if bytes.Equal(body, baseline) {
					t.Fatal("invalid typed-field fixture did not replace its event")
				}
				e, req, fake := responsesTailFixture(t, body, false)
				responsesTailRejected(t, e, req, stream)
				responsesTailSingleFreeCall(t, fake)
			})
		}
	}
}

func TestResponsesTailInvalidDataKeepsExplicitSinglePaidFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := responsesTailBody(nil, []string{`{"type":"ping","cost":null}`, `{"type":"error","error":{"message":"upstream broke"}}`}, false)
			e, req, fake := responsesTailFixture(t, body, true)
			if stream {
				resp, err := e.ExecuteStream(context.Background(), req)
				if err != nil {
					t.Fatalf("explicit paid fallback failed: %v", err)
				}
				if resp.Headers.Get(fallbackHeader) != "paid" {
					t.Fatal("explicit paid fallback marker missing")
				}
				var content string
				for ch := range resp.Chunks {
					if ch.Err != nil {
						t.Fatalf("paid fallback stream failed: %v", ch.Err)
					}
					parsed := freeFixChunk(t, ch.Payload)
					for _, choice := range parsed.Choices {
						content += choice.Delta.Content
					}
				}
				if content != "Paid" {
					t.Fatal("invalid free content leaked into the paid fallback stream")
				}
			} else {
				resp, err := e.Execute(context.Background(), req)
				if err != nil || resp.Headers.Get(fallbackHeader) != "paid" || !bytes.Contains(resp.Payload, []byte(`"Paid"`)) || bytes.Contains(resp.Payload, []byte("Hello Zen")) {
					t.Fatal("invalid free response did not yield exactly one paid completion")
				}
			}
			calls := fake.snapshot()
			if len(calls) != 2 || !strings.HasSuffix(calls[0].URL, "/v1/responses") || !strings.HasSuffix(calls[1].URL, "/v1/chat/completions") {
				t.Fatalf("expected one free call and one paid fallback call, got %d calls", len(calls))
			}
		})
	}
}
