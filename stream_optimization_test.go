package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStreamOptimizationPreservesChatEvents(t *testing.T) {
	payloads := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}],"extra":"preserve"}`,
		`{"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"🙂"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
	}
	var sse strings.Builder
	for _, payload := range payloads {
		sse.WriteString("data: " + payload + "\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			e := freeFixExecutor()
			fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(sse.String())}, nil
			}}
			req := freeFixRequest(fake)
			req.Model = "mimo-free"
			req.Metadata = map[string]any{"request_path": path}
			resp, err := e.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var actual []string
			for chunk := range resp.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				actual = append(actual, string(chunk.Payload))
			}
			if len(actual) != len(payloads) {
				t.Fatalf("chunks = %d, want %d", len(actual), len(payloads))
			}
			for i, expected := range payloads {
				if path == "/v1/messages" {
					expected = "data: " + expected
				}
				if actual[i] != expected {
					t.Fatalf("chunk %d = %q, want %q", i, actual[i], expected)
				}
			}
			if len(fake.snapshot()) != 1 {
				t.Fatal("success unexpectedly called fallback")
			}
		})
	}
}

func TestStreamOptimizationPreservesCompletionExtensions(t *testing.T) {
	completion := []byte(`{"id":"stable","created":123,"choices":[{"message":{"content":"","tool_calls":[{"id":"call_a","type":"function","extra":{"keep":true},"function":{"name":"probe","arguments":"{\"x\":\"你好🙂\"}","extra":"keep"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2}}}`)
	pieces := chunkCompletion("requested", completion, framingBare)
	if len(pieces) != 2 {
		t.Fatalf("pieces = %d, want 2", len(pieces))
	}
	for _, piece := range pieces {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(piece, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["id"]) != `"stable"` || string(raw["created"]) != "123" {
			t.Fatalf("lost response identity: %s", piece)
		}
	}
	if !bytes.Contains(pieces[0], []byte(`"extra":{"keep":true}`)) || !bytes.Contains(pieces[0], []byte(`"extra":"keep"`)) {
		t.Fatalf("lost tool extensions: %s", pieces[0])
	}
	if !bytes.Contains(pieces[1], []byte(`"cached_tokens":2`)) {
		t.Fatalf("lost usage extension: %s", pieces[1])
	}
}

func TestStreamOptimizationLongDeltaRoundTrip(t *testing.T) {
	for _, endpoint := range []string{"chat", "responses"} {
		for _, kind := range []string{"text", "tool"} {
			t.Run(endpoint+"/"+kind, func(t *testing.T) {
				body := benchmarkTextSSE(endpoint, 1024)
				wantContent := strings.Repeat(benchmarkTextDelta(), 1024)
				wantArgs, wantFinish := "", "stop"
				if kind == "tool" {
					body = benchmarkToolSSE(endpoint)
					wantContent = ""
					wantArgs = `{"value":"` + strings.Repeat("a", (64<<10)-len(`{"value":""}`)) + `"}`
					wantFinish = "tool_calls"
				}
				e := freeFixExecutor()
				e.cfg.Free.Quota.FailoverPaid = false
				fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
				}}
				req := freeFixRequest(fake)
				if endpoint == "chat" {
					req.Model = "mimo-free"
				}
				resp, err := e.Execute(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := parseCompletion(resp.Payload)
				if err != nil {
					t.Fatal(err)
				}
				choice := parsed.Choices[0]
				if choice.Message.Content != wantContent || choice.FinishReason != wantFinish {
					t.Fatal("non-stream text or finish reason changed")
				}
				if kind == "tool" {
					if len(choice.Message.ToolCalls) != 1 {
						t.Fatal("non-stream lost tool")
					}
					call := choice.Message.ToolCalls[0]
					if call.ID != "call_1" || call.Function.Name != "probe" || call.Function.Arguments != wantArgs {
						t.Fatal("non-stream tool changed")
					}
				}
				stream, err := e.ExecuteStream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				var text, args strings.Builder
				id, name, finish := "", "", ""
				var usage map[string]int64
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					decoded := freeFixChunk(t, chunk.Payload)
					for _, ch := range decoded.Choices {
						text.WriteString(ch.Delta.Content)
						for _, call := range ch.Delta.ToolCalls {
							if call.Index != 0 {
								t.Fatal("tool index changed")
							}
							if call.ID != "" {
								id = call.ID
							}
							if call.Function.Name != "" {
								name = call.Function.Name
							}
							args.WriteString(call.Function.Arguments)
						}
						if ch.FinishReason != nil {
							finish = *ch.FinishReason
						}
					}
					if decoded.Usage != nil {
						usage = decoded.Usage
					}
				}
				if text.String() != wantContent || args.String() != wantArgs || finish != wantFinish {
					t.Fatal("stream content, arguments or finish reason changed")
				}
				if kind == "tool" && (id != "call_1" || name != "probe") {
					t.Fatal("stream tool identity changed")
				}
				if usage["prompt_tokens"] != 7 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 10 {
					t.Fatalf("stream usage changed: %v", usage)
				}
			})
		}
	}
}

func TestStreamOptimizationValidatesTailBeforeEmission(t *testing.T) {
	for _, endpoint := range []string{"chat", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			body := bytes.Replace(benchmarkTextSSE(endpoint, 256), []byte("data: [DONE]"), []byte("data: {\"type\":\"error\",\"error\":{\"message\":\"late upstream failure\"}}\n\ndata: [DONE]"), 1)
			e := freeFixExecutor()
			e.cfg.Free.Quota.FailoverPaid = false
			fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
			}}
			req := freeFixRequest(fake)
			if endpoint == "chat" {
				req.Model = "mimo-free"
			}
			resp, err := e.ExecuteStream(context.Background(), req)
			if err == nil || resp.Chunks != nil {
				t.Fatal("late upstream error became a successful stream")
			}
			if len(fake.snapshot()) != 1 {
				t.Fatal("disabled fallback dispatched another request")
			}
		})
	}
}

func TestStreamOptimizationSSEOwnsEvents(t *testing.T) {
	for _, frame := range []string{
		"data: {\"x\":1}\n\n",
		"data: {\"x\":\n" + "data: 1}\n\n",
	} {
		body := []byte(frame)
		events, _, err := sseEvents(body)
		if err != nil || len(events) != 1 {
			t.Fatalf("events = %q, err = %v", events, err)
		}
		want := bytes.Clone(events[0])
		for i := range body {
			body[i] = 'x'
		}
		if !bytes.Equal(events[0], want) {
			t.Fatal("event aliases the upstream response buffer")
		}
	}
}

func TestStreamOptimizationPreparedQueueCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pieces := make([][]byte, 1024)
	for i := range pieces {
		pieces[i] = []byte(`{"choices":[]}`)
	}
	chunks := completionChunks(ctx, pieces)
	// Already prepared pieces need no background producer that can get stuck
	// when a caller abandons the response without cancelling its context.
	if len(chunks) != len(pieces) {
		t.Fatalf("prepared queue has %d pieces, want %d without a reader", len(chunks), len(pieces))
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range pieces {
		select {
		case chunk, ok := <-chunks:
			if !ok || !bytes.Equal(chunk.Payload, pieces[0]) {
				t.Fatal("prepared queue lost a piece")
			}
		case <-timer.C:
			t.Fatal("prepared queue blocked")
		}
	}
	select {
	case _, ok := <-chunks:
		if ok {
			t.Fatal("unexpected extra piece")
		}
	case <-timer.C:
		t.Fatal("prepared queue was not closed")
	}
	cancel()
	for range completionChunks(ctx, pieces) {
		t.Fatal("already-cancelled context emitted a piece")
	}
}

func TestStreamOptimizationKeepsValidationScope(t *testing.T) {
	for _, identity := range []string{`"id":123`, `"created":"invalid"`, `"id":123,"id":"ok"`, `"created":"invalid","created":123`} {
		body := []byte(`{` + identity + `,"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		// Non-stream fallback historically passes the raw response through;
		// only stream rendering needs typed identity fields.
		if err := validateCompletion(body); err != nil {
			t.Fatalf("validation scope changed: %v", err)
		}
		if len(chunkCompletion("test", body, framingBare)) != 0 {
			t.Fatalf("invalid stream identity accepted: %s", body)
		}
	}
	for _, body := range []string{
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"},{"message":{"tool_calls":[{"id":123}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":123,"function":{"name":"f","arguments":"{}"}}],"tool_calls":[{"id":"ok","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		if validateCompletion([]byte(body)) == nil || len(chunkCompletion("test", []byte(body), framingBare)) != 0 {
			t.Fatal("tool field type validation was relaxed")
		}
	}
	body := []byte(`{"id":"stable","id":null,"created":123,"created":null,"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	pieces := chunkCompletion("test", body, framingBare)
	if len(pieces) != 2 || !bytes.Contains(pieces[0], []byte(`"id":"stable"`)) || !bytes.Contains(pieces[0], []byte(`"created":123`)) {
		t.Fatal("duplicate null identity no longer follows typed JSON decoding")
	}
}
