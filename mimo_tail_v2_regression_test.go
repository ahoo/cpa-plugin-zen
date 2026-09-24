package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This black-box regression also compiles against the deployed 0.2.4 source.
func TestFreeMimoV2PreservesPerFrameStructure(t *testing.T) {
	type frame struct {
		Kind          string      `json:"kind"`
		Fields        [][2]string `json:"fields"`
		UnknownFields int         `json:"unknown_fields"`
	}
	object := func(fields ...[2]string) frame {
		return frame{Kind: "object", Fields: fields}
	}
	cases := []struct {
		name   string
		tail   []string
		frames []frame
	}{
		{"empty-choices", []string{`{"choices":[],"cost":{"private":"SENTINEL_PRIVATE"}}`}, []frame{
			object([2]string{"choices", "empty_array"}, [2]string{"cost", "opaque"}),
		}},
		{"null-is-not-empty", []string{`{"choices":null,"cost":null}`}, []frame{
			object([2]string{"choices", "null"}, [2]string{"cost", "opaque"}),
		}},
		{"nonempty-output", []string{`{"choices":[{"delta":{"content":"SENTINEL_PRIVATE"}}],"cost":null}`}, []frame{
			object([2]string{"choices", "nonempty_array"}, [2]string{"cost", "opaque"}),
		}},
		{"duplicates-preserve-earlier-output", []string{`{"choices":[{"text":"SENTINEL_PRIVATE"}],"choices":[],"cost":null}`}, []frame{
			object([2]string{"choices", "nonempty_array"}, [2]string{"choices", "empty_array"}, [2]string{"cost", "opaque"}),
		}},
		{"invalid-type-not-overwritten", []string{`{"type":7,"type":"ping","cost":null}`}, []frame{
			object([2]string{"type", "invalid"}, [2]string{"type", "ping"}, [2]string{"cost", "opaque"}),
		}},
		{"frame-order-not-collapsed", []string{`{"choices":[],"cost":null}`, `{"text":"SENTINEL_PRIVATE"}`}, []frame{
			object([2]string{"choices", "empty_array"}, [2]string{"cost", "opaque"}),
			object([2]string{"text", "nonempty_string"}),
		}},
		{"reverse-frame-order", []string{`{"text":"SENTINEL_PRIVATE"}`, `{"choices":[],"cost":null}`}, []frame{
			object([2]string{"text", "nonempty_string"}),
			object([2]string{"choices", "empty_array"}, [2]string{"cost", "opaque"}),
		}},
		{"unknown-names-redacted", []string{`{"SENTINEL_PRIVATE":{"private":"SENTINEL_PRIVATE"},"cost":null}`}, []frame{
			{Kind: "object", Fields: [][2]string{{"cost", "opaque"}}, UnknownFields: 1},
		}},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
				e := geoDiagnosticExecutor(t, path, false, "chat")
				lines := []string{`data: {"choices":[{"index":0,"delta":{"content":"Ready"},"finish_reason":"stop"}]}`, `data: [DONE]`}
				for _, payload := range tc.tail {
					lines = append(lines, "data: "+payload)
				}
				body := freeFixSSE(lines...)
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
				}}
				err := availabilityRequestError(t, e, geoDiagnosticRequest(fake, "mimo-free"), stream)
				availabilityStatus(t, err, http.StatusBadGateway)
				const marker = "; tail_structure_v2="
				const prefix = "zen free executor: data after SSE termination"
				message := err.Error()
				if !strings.HasPrefix(message, prefix+marker) || strings.Contains(message, "SENTINEL_PRIVATE") {
					t.Fatal("strict Mimo failure needs a v2 structural diagnostic without private input")
				}
				raw := strings.TrimPrefix(message, prefix+marker)
				if len(marker)+len(raw) > 1024 {
					t.Fatal("entire v2 suffix exceeds its limit")
				}
				var envelope struct {
					Schema         int             `json:"schema"`
					Summary        json.RawMessage `json:"summary"`
					DetailComplete bool            `json:"detail_complete"`
					Frames         []frame         `json:"frames"`
				}
				if json.Unmarshal([]byte(raw), &envelope) != nil || envelope.Schema != 2 || !envelope.DetailComplete || !reflect.DeepEqual(envelope.Frames, tc.frames) {
					t.Fatalf("wrong per-frame structure: %s", raw)
				}
				var got, want map[string]any
				if json.Unmarshal(envelope.Summary, &got) != nil || json.Unmarshal([]byte(summarizeMimoSSETail(body)), &want) != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("v2 changed the original summary counters")
				}
				if len(fake.snapshot()) != 1 {
					t.Fatal("diagnostic dispatched extra free or paid requests")
				}
			})
		}
	}
}
