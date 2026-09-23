package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSystemOneStreamRetainsUsage(t *testing.T) {
	host := &recordingHostHTTPClient{response: pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"answers":{"ok":true},"usage":{"input_tokens":7,"output_tokens":3}}`),
	}}
	e := &Executor{cfg: testConfig(), keypool: newPool()}
	resp, err := e.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{
		Model: "jev-free", HTTPClient: host,
		Payload: []byte(`{"messages":[{"role":"user","content":"urgent?"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []map[string]json.RawMessage
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(chunk.Payload, &got); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, got)
	}
	// Host v7.3.13 resets usage when it initializes the first Responses
	// event. Content and terminal usage must not share that first chunk.
	if len(chunks) < 2 || len(chunks[0]["usage"]) != 0 {
		t.Fatalf("usage must follow stream initialization: %v", chunks)
	}
	var usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	}
	if err := json.Unmarshal(chunks[len(chunks)-1]["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Prompt != 7 || usage.Completion != 3 || usage.Total != 10 {
		t.Fatalf("stream lost token usage: %+v", usage)
	}
}
