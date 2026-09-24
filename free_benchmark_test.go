package plugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Each text delta is 64 bytes without splitting a UTF-8 rune.
func benchmarkTextDelta() string { return strings.Repeat("你好🙂", 6) + "abcd" }

func benchmarkTextSSE(endpoint string, count int) []byte {
	var body strings.Builder
	delta := benchmarkTextDelta()
	for i := 0; i < count; i++ {
		var event any
		if endpoint == "responses" {
			event = map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "delta": delta}
		} else {
			event = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}}}
		}
		body.WriteString("data: ")
		body.Write(freeFixJSON(event))
		body.WriteString("\n\n")
	}
	if endpoint == "responses" {
		body.WriteString("data: ")
		body.Write(freeFixJSON(map[string]any{"type": "response.output_text.done", "item_id": "msg_1", "text": strings.Repeat(delta, count)}))
		body.WriteString("\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	} else {
		body.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return []byte(body.String())
}

func benchmarkToolSSE(endpoint string) []byte {
	args := `{"value":"` + strings.Repeat("a", (64<<10)-len(`{"value":""}`)) + `"}`
	var body strings.Builder
	write := func(event any) {
		body.WriteString("data: ")
		body.Write(freeFixJSON(event))
		body.WriteString("\n\n")
	}
	if endpoint == "responses" {
		write(map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "fc_1", "call_id": "call_1", "type": "function_call", "name": "probe", "arguments": ""}})
	} else {
		write(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "probe", "arguments": ""}}}}}}})
	}
	for start := 0; start < len(args); start += 64 {
		delta := args[start:min(start+64, len(args))]
		if endpoint == "responses" {
			write(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": delta})
		} else {
			write(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": delta}}}}}}})
		}
	}
	if endpoint == "responses" {
		write(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_1", "arguments": args})
		body.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	} else {
		body.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return []byte(body.String())
}

func BenchmarkFreeExecuteStream(b *testing.B) {
	for _, endpoint := range []string{"chat", "responses"} {
		for _, count := range []int{256, 1024} {
			b.Run(fmt.Sprintf("%s/%d", endpoint, count), func(b *testing.B) {
				body := benchmarkTextSSE(endpoint, count)
				e := freeFixExecutor()
				e.cfg.Free.Quota.FailoverPaid = false
				model := "mimo-free"
				if endpoint == "responses" {
					model = "muse-free"
				}
				// This existing fake retains only the latest request, not every iteration.
				host := &recordingHostHTTPClient{response: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}}
				req := pluginapi.ExecutorRequest{Model: model, HTTPClient: host, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)}
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					resp, err := e.ExecuteStream(context.Background(), req)
					if err != nil {
						b.Fatal(err)
					}
					chunks := 0
					for chunk := range resp.Chunks {
						if chunk.Err != nil {
							b.Fatal(chunk.Err)
						}
						chunks++
					}
					if chunks != count+1 {
						b.Fatalf("chunks = %d, want %d", chunks, count+1)
					}
				}
			})
		}
	}
}

func BenchmarkFreeAssembly(b *testing.B) {
	for _, tc := range []struct {
		name, endpoint string
		tools          bool
	}{
		{"ResponsesText64KiB", "responses", false},
		{"ResponsesToolArgs64KiB", "responses", true},
		{"ChatToolArgs64KiB", "chat", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := benchmarkTextSSE(tc.endpoint, 1024)
			if tc.tools {
				body = benchmarkToolSSE(tc.endpoint)
			}
			assemble := assembleResponsesCompletion
			if tc.endpoint == "chat" {
				assemble = assembleChatCompletion
			}
			completion, err := assemble("bench", body)
			if err != nil {
				b.Fatal(err)
			}
			if err := validateCompletion(completion); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(64 << 10)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := assemble("bench", body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFreeChunkCompletion64KiB(b *testing.B) {
	body := freeFixJSON(map[string]any{
		"id": "bench", "created": 123,
		"choices": []any{map[string]any{"message": map[string]any{"content": strings.Repeat(benchmarkTextDelta(), 1024)}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
	})
	b.ReportAllocs()
	b.SetBytes(64 << 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(chunkCompletion("bench", body, framingBare)) < 2 {
			b.Fatal("completion did not produce content and terminal usage")
		}
	}
}
