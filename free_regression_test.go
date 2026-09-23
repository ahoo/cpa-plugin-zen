package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type freeFixFakeHTTP struct {
	mu     sync.Mutex
	calls  []pluginapi.HTTPRequest
	answer func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *freeFixFakeHTTP) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.answer(ctx, req)
}
func (f *freeFixFakeHTTP) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("unexpected streaming HTTP call")
}
func (f *freeFixFakeHTTP) snapshot() []pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginapi.HTTPRequest(nil), f.calls...)
}

func freeFixExecutor() *Executor {
	cfg := parseConfig([]byte(`paid:
  api_keys:
    - key: sk-test
  base_url: https://example.invalid
free:
  enabled: true
  members:
    - key: public
  quota:
    failover_paid: true
    paid_fallback: paid-model
`))
	return &Executor{cfg: cfg, keypool: newPool(), freepool: newFreePool(cfg), sessions: &sessionPool{}}
}
func freeFixRequest(fake *freeFixFakeHTTP) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Model: "muse-free", HTTPClient: fake,
		Payload: []byte(`{"model":"muse-free","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)}
}
func freeFixSSE(lines ...string) []byte { return []byte(strings.Join(lines, "\n\n") + "\n\n") }
func freeFixChunk(t *testing.T, raw []byte) struct {
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
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]int64 `json:"usage"`
} {
	t.Helper()
	var parsed struct {
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
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("data: ")), &parsed); err != nil {
		t.Fatalf("invalid chunk %q: %v", raw, err)
	}
	return parsed
}

func TestFreeFixFallbackSingleCallAndShape(t *testing.T) {
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if req.URL == "https://example.invalid/v1/responses" {
			return pluginapi.HTTPResponse{StatusCode: 503, Body: []byte(`{"error":"unavailable"}`)}, nil
		}
		if req.URL != "https://example.invalid/v1/chat/completions" {
			t.Fatalf("wrong URL: %s", req.URL)
		}
		if req.Method != http.MethodPost {
			t.Errorf("fallback method = %s", req.Method)
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != false || body["stream_options"] != nil {
			t.Errorf("fallback must force nonstream and remove stream_options: %s", req.Body)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"Paid"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)}, nil
	}}
	req := freeFixRequest(fake)
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Headers.Get(fallbackHeader) != "paid" {
		t.Fatalf("fallback header missing: %v", resp.Headers)
	}
	if !bytes.Contains(resp.Payload, []byte(`"Paid"`)) {
		t.Fatalf("paid completion mapped as responses: %s", resp.Payload)
	}
	if got := len(fake.snapshot()); got != 2 {
		t.Fatalf("expected one free, one paid call, got %d", got)
	}
}

func TestFreeFixRequestErrorsDoNotFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"bad request", 400, `{"error":"invalid payload"}`}, {"unsupported", 422, `{"error":"unsupported model"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			e := freeFixExecutor()
			fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}, nil
			}}
			_, err := e.Execute(context.Background(), freeFixRequest(fake))
			var se statusError
			if !errors.As(err, &se) || se.StatusCode() != tc.status {
				t.Fatalf("expected real status %d, got %v", tc.status, err)
			}
			if got := len(fake.snapshot()); got != 1 {
				t.Fatalf("deterministic error must not fallback, calls=%d", got)
			}
		})
	}
}

func TestFreeFixChatToolAssemblyAndTermination(t *testing.T) {
	sse := freeFixSSE(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"p\":"}},{"index":1,"id":"call_b","type":"function","function":{"name":"write","arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"你好\"}"}},{"index":1,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	out, err := assembleChatCompletion("mimo-free", sse)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
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
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || got.Choices[0].FinishReason != "tool_calls" || len(got.Choices[0].Message.ToolCalls) != 2 || got.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"p":"你好"}` || got.Choices[0].Message.ToolCalls[1].ID != "call_b" {
		t.Fatalf("lost tool calls: %s", out)
	}
	for _, invalid := range [][]byte{nil, freeFixSSE(`data: {"choices":[{"delta":{"content":"unfinished"}}]}`), freeFixSSE(`data: {"error":{"message":"broken"}}`)} {
		if result, err := assembleChatCompletion("mimo-free", invalid); err == nil {
			t.Errorf("invalid/truncated chat stream accepted: %s", result)
		}
	}
}

func TestFreeFixResponsesDoneDedupAndToolFinish(t *testing.T) {
	sse := freeFixSSE(
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"a\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hi\"}",
		`data: {"type":"response.output_text.done","item_id":"a","output_index":0,"content_index":0,"text":"Hi"}`,
		`data: {"type":"response.output_text.done","item_id":"b","output_index":1,"content_index":0,"text":"你好"}`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_x","call_id":"call_x","type":"function_call","name":"read"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_x","delta":"{}"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":4,"output_tokens":7}}}`)
	out, err := assembleResponsesCompletion("muse-free", sse)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int64 `json:"usage"`
	}
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "Hi你好" || len(got.Choices[0].Message.ToolCalls) != 1 || got.Choices[0].FinishReason != "tool_calls" || got.Usage["total_tokens"] != 11 {
		t.Fatalf("incorrect responses conversion: %s", out)
	}
	if call, ok := got.Choices[0].Message.ToolCalls[0].(map[string]any); !ok || call["id"] != "call_x" {
		t.Fatalf("tool id must use call_id, not stream item id: %s", out)
	}
	for _, broken := range [][]byte{freeFixSSE(`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"upstream broke"}}}`), freeFixSSE(`data: {"type":"response.output_text.delta","delta":"unfinished"}`)} {
		if result, err := assembleResponsesCompletion("muse-free", broken); err == nil {
			t.Errorf("failed/incomplete response accepted: %s", result)
		}
	}
}

func TestFreeFixChunkCompletionUTF8ToolsUsage(t *testing.T) {
	text := strings.Repeat("x", 499) + "😀你好"
	body := []byte(`{"choices":[{"message":{"content":` + string(freeFixJSON(text)) + `,"tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":6,"total_tokens":9}}`)
	chunks := chunkCompletion("muse-free", body, framingBare)
	var result string
	var toolIndex int
	var finish string
	var tokens int64
	for _, raw := range chunks {
		if !utf8.Valid(raw) {
			t.Fatalf("invalid UTF8: %q", raw)
		}
		c := freeFixChunk(t, raw)
		if len(c.Choices) > 0 {
			result += c.Choices[0].Delta.Content
			for _, call := range c.Choices[0].Delta.ToolCalls {
				toolIndex = call.Index + 1
			}
			if c.Choices[0].FinishReason != nil {
				finish = *c.Choices[0].FinishReason
			}
		}
		if c.Usage != nil {
			tokens = c.Usage["total_tokens"]
		}
	}
	if result != text || toolIndex != 1 || finish != "tool_calls" || tokens != 9 {
		t.Fatalf("split completion lost content/tools/finish/usage: result=%q index=%d finish=%s usage=%d", result, toolIndex, finish, tokens)
	}
	if out := chunkCompletion("muse-free", []byte(`{"choices":[{"message":{},"finish_reason":"stop"}]}`), framingBare); len(out) != 0 {
		t.Fatalf("empty invalid completion must not be successful: %q", out)
	}
}
func freeFixJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

func TestFreeFixMultilineSSEAndErrorPropagation(t *testing.T) {
	ctx := context.Background()
	sse := []byte("event: message\ndata: {\"choices\":[{\"delta\":\ndata: {\"content\":\"你好\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	var chunks []pluginapi.ExecutorStreamChunk
	for ch := range forwardChatSSE(ctx, sse, framingBare) {
		chunks = append(chunks, ch)
	}
	if len(chunks) != 1 || len(chunks[0].Payload) == 0 || freeFixChunk(t, chunks[0].Payload).Choices[0].Delta.Content != "你好" {
		t.Fatalf("multiline event not parsed: %+v", chunks)
	}
	for ch := range forwardChatSSE(ctx, freeFixSSE(`data: {"error":{"message":"bad"}}`), framingBare) {
		if ch.Err == nil {
			t.Fatalf("error event forwarded as success: %+v", ch)
		}
		return
	}
	t.Fatal("explicit chat stream error swallowed")
}

func TestFreeFixFallbackFailureNeverStops(t *testing.T) {
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/v1/responses") {
			return pluginapi.HTTPResponse{StatusCode: 503, Body: []byte(`{"error":"unavailable"}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 502, Body: []byte(`{"error":"paid down"}`)}, nil
	}}
	resp, err := e.ExecuteStream(context.Background(), freeFixRequest(fake))
	if err == nil {
		for chunk := range resp.Chunks {
			if chunk.Err == nil {
				t.Fatalf("fallback failure fabricated success: %s", chunk.Payload)
			}
			err = chunk.Err
		}
	}
	if err == nil {
		t.Fatal("missing real fallback error")
	}
}

func TestFreeFixResponsesToolRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"muse-free","messages":[{"role":"user","content":"read"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"contents"}]}`)
	body, err := buildResponsesBody(raw, "muse-free", nil, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"call_id":"call_1"`)) || !bytes.Contains(body, []byte(`"type":"function_call_output"`)) || !bytes.Contains(body, []byte(`"type":"function_call"`)) {
		t.Fatalf("tool roundtrip silently dropped: %s", body)
	}
	for _, invalid := range []string{
		`{"messages":[{"role":"tool","content":"result"}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{bad"}}]}]}`,
	} {
		if result, err := buildResponsesBody([]byte(invalid), "muse-free", nil, 16); err == nil {
			t.Errorf("malformed tool request silently converted: %s", result)
		}
	}
}

func TestFreeFixResponsesToolDoneDoesNotRepeat(t *testing.T) {
	sse := freeFixSSE(
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"read"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"a\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"path\":\"a\"}"}`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","call_id":"call_1","type":"function_call","name":"read","arguments":"{\"path\":\"a\"}"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`)
	mapped, err := assembleResponsesCompletion("muse-free", sse)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(mapped, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 || got.Choices[0].Message.ToolCalls[0].ID != "call_1" || got.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"path":"a"}` {
		t.Fatalf("duplicate tool arguments: %s", mapped)
	}
	var fromStream strings.Builder
	for chunk := range convertResponsesSSE(context.Background(), sse, framingBare) {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		parsed := freeFixChunk(t, chunk.Payload)
		for _, choice := range parsed.Choices {
			for _, call := range choice.Delta.ToolCalls {
				fromStream.WriteString(call.Function.Arguments)
			}
		}
	}
	if fromStream.String() != `{"path":"a"}` {
		t.Fatalf("stream duplicated tool arguments: %q", fromStream.String())
	}
}

func TestFreeFixEmptyFreeStreamFallbackAndUsage(t *testing.T) {
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/v1/responses") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: freeFixSSE(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":0}}}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"已处理😀"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)}, nil
	}}
	resp, err := e.ExecuteStream(context.Background(), freeFixRequest(fake))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Headers.Get(fallbackHeader) != "paid" {
		t.Fatalf("missing paid fallback marker: %v", resp.Headers)
	}
	var text, finish string
	var usage int64
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		parsed := freeFixChunk(t, chunk.Payload)
		for _, choice := range parsed.Choices {
			text += choice.Delta.Content
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
		usage = parsed.Usage["total_tokens"]
	}
	if text != "已处理😀" || finish != "stop" || usage != 6 || len(fake.snapshot()) != 2 {
		t.Fatalf("empty free stream fallback lost content/usage: text=%q finish=%q usage=%d calls=%d", text, finish, usage, len(fake.snapshot()))
	}
}

func TestFreeFixCancellationBeforeStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("canceled request reached upstream")
		return pluginapi.HTTPResponse{}, nil
	}}
	if _, err := e.ExecuteStream(ctx, freeFixRequest(fake)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must bypass fallback: %v", err)
	}
	if len(fake.snapshot()) != 0 {
		t.Fatal("canceled request was sent")
	}
	for chunk := range completionChunks(ctx, [][]byte{[]byte(`{"choices":[{"delta":{"content":"never"}}]}`)}) {
		t.Fatalf("producer emitted after cancellation: %s", chunk.Payload)
	}
}

func TestFreeFixCancellationDuringFreeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		cancel()
		return pluginapi.HTTPResponse{StatusCode: 200, Body: freeFixSSE(`data: {"type":"response.output_text.done","text":"unused"}`, `data: {"type":"response.completed","response":{"status":"completed"}}`)}, nil
	}}
	_, err := e.ExecuteStream(ctx, freeFixRequest(fake))
	if !errors.Is(err, context.Canceled) || len(fake.snapshot()) != 1 {
		t.Fatalf("cancellation during upstream must stop without paid retry: err=%v calls=%d", err, len(fake.snapshot()))
	}
}

func TestFreeFixEmptyWithoutPaidFallback(t *testing.T) {
	for _, upstream := range [][]byte{
		freeFixSSE(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2}}}`),
		freeFixSSE(`data: {"type":"response.created"}`, `data: {"type":"response.completed","response":{"status":"completed"}}`),
	} {
		e := freeFixExecutor()
		e.cfg.Free.Quota.FailoverPaid = false
		fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: upstream}, nil
		}}
		resp, err := e.ExecuteStream(context.Background(), freeFixRequest(fake))
		if err == nil {
			for chunk := range resp.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				} else {
					t.Fatalf("metadata-only stream fabricated success: %s", chunk.Payload)
				}
			}
		}
		if err == nil || len(fake.snapshot()) != 1 {
			t.Fatalf("empty stream without fallback must error, calls=%d err=%v", len(fake.snapshot()), err)
		}
	}
}

func TestFreeFixDataAfterSSETerminationRejected(t *testing.T) {
	for _, trailing := range []string{`data: {"choices":[{"delta":{"content":"late"},"finish_reason":"stop"}]}`, `data: corrupt`, `data: [DONE]`} {
		sse := freeFixSSE(`data: {"choices":[{"delta":{"content":"early"},"finish_reason":"stop"}]}`, `data: [DONE]`, trailing)
		if result, err := assembleChatCompletion("mimo-free", sse); err == nil {
			t.Errorf("accepted event after [DONE]: %s", result)
		}
		for chunk := range forwardChatSSE(context.Background(), sse, framingBare) {
			if chunk.Err == nil {
				t.Fatalf("emitted success before spotting event after [DONE]: %s", chunk.Payload)
			}
		}
	}
}

func TestFreeFixEndpointAndModeMatrix(t *testing.T) {
	cases := []struct {
		model, path, text string
		body              []byte
	}{
		{"mimo-free", "/v1/chat/completions", "你好😀", freeFixSSE(`data: {"choices":[{"delta":{"content":"你好😀"},"finish_reason":"stop"}]}`, `data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, `data: [DONE]`)},
		{"muse-free", "/v1/responses", "你好😀", freeFixSSE(`data: {"type":"response.output_text.done","item_id":"x","text":"你好😀"}`, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":3}}}`)},
		{"jev-free", "/v1/systemone", `{"x":{"type":"noul","noul":1}}`, []byte(`{"answers":{"x":{"type":"noul","noul":1}},"usage":{"input_tokens":2,"output_tokens":3}}`)},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(tc.model+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				e := freeFixExecutor()
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					if !strings.HasSuffix(req.URL, tc.path) || req.Method != http.MethodPost {
						t.Errorf("wrong free upstream route/method: %+v", req)
					}
					return pluginapi.HTTPResponse{StatusCode: 200, Body: tc.body}, nil
				}}
				req := freeFixRequest(fake)
				req.Model = tc.model
				if tc.model == "jev-free" {
					req.Payload = []byte(`{"model":"jev-free","messages":[{"role":"user","content":"{\"state\":\"hi\",\"questions\":{\"x\":{\"type\":\"noul\",\"instructions\":\"x\"}}}"}]}`)
				}
				if !stream {
					resp, err := e.Execute(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					var parsed struct {
						Choices []struct {
							Message struct {
								Content string `json:"content"`
							} `json:"message"`
						} `json:"choices"`
						Usage map[string]int64 `json:"usage"`
					}
					if err := json.Unmarshal(resp.Payload, &parsed); err != nil {
						t.Fatal(err)
					}
					if resp.Headers.Get(fallbackHeader) != "" || len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != tc.text || parsed.Usage["total_tokens"] != 5 {
						t.Fatalf("free response incorrect: %s", resp.Payload)
					}
				} else {
					resp, err := e.ExecuteStream(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					var content, finish string
					var tokens int64
					for ch := range resp.Chunks {
						if ch.Err != nil {
							t.Fatal(ch.Err)
						}
						parsed := freeFixChunk(t, ch.Payload)
						for _, choice := range parsed.Choices {
							content += choice.Delta.Content
							if choice.FinishReason != nil {
								finish = *choice.FinishReason
							}
						}
						if parsed.Usage != nil {
							tokens = parsed.Usage["total_tokens"]
						}
					}
					if content != tc.text || finish != "stop" || tokens != 5 {
						t.Fatalf("free stream lost content/usage/finish: text=%q finish=%q usage=%d", content, finish, tokens)
					}
				}
				if len(fake.snapshot()) != 1 {
					t.Fatalf("unexpected fallback calls: %+v", fake.snapshot())
				}
			})
		}
	}
}

func TestFreeFixConcurrentFreeMemberSnapshot(t *testing.T) {
	e := freeFixExecutor()
	fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":"bad request"}`)}, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				// Keep the member eligible while exercising concurrent health updates.
				e.freepool.cool(0, 1)
				e.freepool.mu.Lock()
				e.freepool.members[0].cooldownUntil = time.Time{}
				e.freepool.mu.Unlock()
				_, _, _, _ = e.freeCall(context.Background(), freeFixRequest(fake), FreeModelEntry{Endpoint: "chat", Name: "mimo-free"})
			}
		}()
	}
	wg.Wait()
}
