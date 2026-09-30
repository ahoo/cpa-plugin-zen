package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const geoDiagnosticCountry = "This model is not available in your country"
const geoDiagnosticTailPrefix = "zen free executor: data after SSE termination"
const geoDiagnosticTailMarker = "; tail_structure_v2="

func geoDiagnosticExecutor(t *testing.T, path string, fallback bool, mimoEndpoint string) *Executor {
	t.Helper()
	if mimoEndpoint == "" {
		mimoEndpoint = "chat"
	}
	cfg, err := parseConfigStrict([]byte(fmt.Sprintf(`paid:
  api_keys:
    - key: sk-test
  base_url: https://example.invalid
free:
  enabled: true
  session_pool: %q
  members:
    - key: public
    - key: public
  models:
    - alias: mimo-free
      name: syntheticFreeChatEntry
      endpoint: %s
    - alias: ling-free
      name: syntheticOtherChatEntry
      endpoint: chat
    - alias: muse-free
      name: syntheticResponsesEntry
      endpoint: responses
  quota:
    failover_paid: %t
    paid_fallback: paid-model
`, path, mimoEndpoint, fallback)))
	if err != nil {
		t.Fatalf("invalid isolated test config: %v", err)
	}
	return NewExecutor(cfg)
}

func geoDiagnosticRequest(fake *freeFixFakeHTTP, model string) pluginapi.ExecutorRequest {
	req := freeFixRequest(fake)
	req.Model = model
	req.Payload = []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, model))
	return req
}

func geoDiagnosticSeedAvailableHealth(e *Executor) {
	// Nonzero, naturally expired health distinguishes a harmless 403 from
	// report(success), report(failure), and member cool() side effects.
	e.freepool.mu.Lock()
	for i := range e.freepool.members {
		e.freepool.members[i].fails = 2
		e.freepool.members[i].cooldownUntil = time.Now().Add(-time.Minute)
	}
	e.freepool.mu.Unlock()
}

func geoDiagnosticChatSSE() []byte {
	return freeFixSSE(
		`data: {"choices":[{"index":0,"delta":{"content":"Recovered"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
}

func geoDiagnosticMimoTailSSE() []byte {
	return freeFixSSE(
		`data: {"choices":[{"index":0,"delta":{"content":"Ready"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		`data: {"type":"ping","cost":{"units":"diagnostic-cost-sentinel"}}`,
	)
}

func geoDiagnosticResponsesTailSSE() []byte {
	return freeFixSSE(
		`data: {"type":"response.output_text.done","item_id":"out","text":"Ready"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		`data: [DONE]`,
		`data: {"type":"ping","cost":null}`,
	)
}

func geoDiagnosticAssertMimoRequest(t *testing.T, upstream pluginapi.HTTPRequest) {
	t.Helper()
	if upstream.Method != http.MethodPost || !strings.HasSuffix(upstream.URL, "/v1/chat/completions") {
		t.Fatal("mimo-free fixture must actually send a chat completion")
	}
	var body struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Stream   bool              `json:"stream"`
	}
	if err := json.Unmarshal(upstream.Body, &body); err != nil || body.Model != "syntheticFreeChatEntry" || len(body.Messages) == 0 || !body.Stream {
		t.Fatal("mimo-free fixture must send the synthetic upstream chat model and messages")
	}
}

func geoDiagnosticAssertSummary(t *testing.T, err error, want bool) {
	t.Helper()
	availabilityStatus(t, err, http.StatusBadGateway)
	message := err.Error()
	if !strings.HasPrefix(message, geoDiagnosticTailPrefix) {
		t.Fatal("unexpected SSE termination error prefix")
	}
	if !want {
		if message != geoDiagnosticTailPrefix {
			t.Fatal("non-Mimo/non-chat tail error must not contain a diagnostic suffix")
		}
		return
	}
	if strings.Count(message, geoDiagnosticTailMarker) != 1 || !strings.HasPrefix(message, geoDiagnosticTailPrefix+geoDiagnosticTailMarker) {
		t.Fatal("first Mimo tail error must contain exactly one diagnostic suffix")
	}
	raw := strings.TrimPrefix(message, geoDiagnosticTailPrefix+geoDiagnosticTailMarker)
	if !json.Valid([]byte(raw)) {
		t.Fatal("Mimo tail summary is not valid JSON")
	}
	var decoded any
	if json.Unmarshal([]byte(raw), &decoded) != nil || decoded == nil || strings.Contains(raw, "diagnostic-cost-sentinel") {
		t.Fatal("Mimo tail summary must be structural JSON without raw cost values")
	}
}

func TestFreeGeoCountryCoolsBlockedEgressAndFailsOver(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
			e := geoDiagnosticExecutor(t, path, false, "chat")
			geoDiagnosticSeedAvailableHealth(e)
			beforeSessions, _ := availabilityHealth(e)
			calls := 0
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(request.URL, "/v1/responses") {
					t.Fatal("muse-free failover must stay on the responses endpoint")
				}
				calls++
				if calls == 1 {
					return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(geoDiagnosticCountry)}, nil
				}
				return availabilityFreeSuccess(request), nil
			}}
			if marker := availabilityResponse(t, e, geoDiagnosticRequest(fake, "muse-free"), stream, "Recovered").Get(fallbackHeader); marker != "" {
				t.Fatal("free egress failover must not use paid fallback")
			}
			got := fake.snapshot()
			if len(got) != 2 {
				t.Fatalf("blocked egress must fail over to the next member, got %d calls", len(got))
			}
			for _, c := range got {
				if !strings.HasSuffix(c.URL, "/v1/responses") || c.Headers.Get("X-Opencode-Session") == "" {
					t.Fatal("failover must reuse the responses endpoint with a session")
				}
			}
			afterSessions, afterMembers := availabilityHealth(e)
			if !reflect.DeepEqual(afterSessions, beforeSessions) {
				t.Fatal("geographic failover must not penalize sessions")
			}
			cooled := 0
			for _, m := range afterMembers {
				switch m.fails {
				case 3:
					cooled++
					if !m.until.After(time.Now().Add(time.Hour)) {
						t.Fatal("policy-denied member must cool long, not 300s")
					}
				case 2:
				default:
					t.Fatalf("unexpected member health: %+v", m)
				}
			}
			if cooled != 1 {
				t.Fatalf("exactly one blocked egress must cool, got %d", cooled)
			}
		})
	}
}

func TestFreeGeoCountryAllBlockedPreservesLastResort(t *testing.T) {
	cases := []struct{ name, body string }{
		{"plain", geoDiagnosticCountry},
		{"console-prefix", "Error from provider (Console): RegionError: " + geoDiagnosticCountry},
		{"error-message", `{"error":{"message":"RegionError: ` + geoDiagnosticCountry + `"}}`},
		{"mixed-case-and-busy", `{"error":{"message":"regionerror: THIS MODEL IS NOT AVAILABLE IN YOUR COUNTRY; backend busy, try again"}}`},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				path := availabilityPoolFile(t, availabilitySessions(t, 2, 2, time.Now().Add(-time.Minute).Unix()))
				e := geoDiagnosticExecutor(t, path, false, "chat")
				geoDiagnosticSeedAvailableHealth(e)
				beforeDisk := availabilitySnapshotFile(t, path)
				beforeSessions, _ := availabilityHealth(e)
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(tc.body)}, nil
				}}
				err := availabilityRequestError(t, e, geoDiagnosticRequest(fake, "muse-free"), stream)
				availabilityStatus(t, err, http.StatusForbidden)
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(geoDiagnosticCountry)) {
					t.Fatal("must return the original country-restriction message")
				}
				calls := fake.snapshot()
				if len(calls) != 2 {
					t.Fatalf("all-blocked pool must try every egress once, got %d calls", len(calls))
				}
				for _, c := range calls {
					if !strings.HasSuffix(c.URL, "/v1/responses") || c.Headers.Get("X-Opencode-Session") == "" {
						t.Fatal("failover must stay on responses with a session")
					}
				}
				afterSessions, afterMembers := availabilityHealth(e)
				if !reflect.DeepEqual(afterSessions, beforeSessions) {
					t.Fatal("geographic denial must not penalize sessions")
				}
				cooled, pristine := 0, 0
				for _, m := range afterMembers {
					switch m.fails {
					case 3:
						cooled++
						if !m.until.After(time.Now().Add(time.Hour)) {
							t.Fatal("policy-denied member must cool long, not 300s")
						}
					case 2:
						pristine++
					default:
						t.Fatalf("unexpected member health: %+v", m)
					}
				}
				if cooled != 1 || pristine != 1 {
					t.Fatalf("last-resort egress must stay uncooled, got cooled=%d pristine=%d", cooled, pristine)
				}
				availabilityFileUnchanged(t, path, beforeDisk)
			})
		}
	}
}

func TestFreeGeoExplicitPaidFallbackKeepsOneSequence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 2, 2, time.Now().Add(-time.Minute).Unix()))
			e := geoDiagnosticExecutor(t, path, true, "chat")
			geoDiagnosticSeedAvailableHealth(e)
			beforeDisk := availabilitySnapshotFile(t, path)
			beforeSessions, _ := availabilityHealth(e)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if strings.HasSuffix(request.URL, "/v1/responses") {
					return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(`{"error":{"message":"` + geoDiagnosticCountry + `"}}`)}, nil
				}
				if !strings.HasSuffix(request.URL, "/v1/chat/completions") {
					t.Fatal("unexpected fallback endpoint")
				}
				return availabilityPaidSuccess(), nil
			}}
			if got := availabilityResponse(t, e, geoDiagnosticRequest(fake, "muse-free"), stream, "Paid").Get(fallbackHeader); got != "paid" {
				t.Fatal("explicit paid fallback must mark its response")
			}
			calls := fake.snapshot()
			if len(calls) != 3 || !strings.HasSuffix(calls[0].URL, "/v1/responses") || !strings.HasSuffix(calls[1].URL, "/v1/responses") || !strings.HasSuffix(calls[2].URL, "/v1/chat/completions") {
				t.Fatalf("expected two free egress attempts then one paid resolver call, got %d", len(calls))
			}
			afterSessions, afterMembers := availabilityHealth(e)
			if !reflect.DeepEqual(afterSessions, beforeSessions) {
				t.Fatal("geographic denial must not penalize sessions")
			}
			cooled, pristine := 0, 0
			for _, m := range afterMembers {
				switch m.fails {
				case 3:
					cooled++
					if !m.until.After(time.Now().Add(time.Hour)) {
						t.Fatal("policy-denied member must cool long, not 300s")
					}
				case 2:
					pristine++
				default:
					t.Fatalf("unexpected member health: %+v", m)
				}
			}
			if cooled != 1 || pristine != 1 {
				t.Fatalf("last-resort egress must stay uncooled, got cooled=%d pristine=%d", cooled, pristine)
			}
			availabilityFileUnchanged(t, path, beforeDisk)
		})
	}
}

func TestFreeGeoOtherStatusesAndUnknown403StillRetry(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"unknown-403", `{"error":{"message":"RegionError: country gate is busy; try again"}}`, http.StatusForbidden},
		{"partial-phrase-403", `{"error":{"message":"This model is not available in your region; backend busy"}}`, http.StatusForbidden},
		{"country-phrase-429", `{"error":{"message":"` + geoDiagnosticCountry + `"}}`, http.StatusTooManyRequests},
		{"country-phrase-503", geoDiagnosticCountry, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				path := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
				e := geoDiagnosticExecutor(t, path, false, "chat")
				attempts := 0
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					if !strings.HasSuffix(request.URL, "/v1/chat/completions") {
						t.Fatal("retry control must stay on free chat endpoint")
					}
					attempts++
					if attempts == 1 {
						return pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}, nil
					}
					return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: geoDiagnosticChatSSE()}, nil
				}}
				if marker := availabilityResponse(t, e, geoDiagnosticRequest(fake, "mimo-free"), stream, "Recovered").Get(fallbackHeader); marker != "" {
					t.Fatal("ordinary free retry must not use paid fallback")
				}
				if attempts != 2 || len(fake.snapshot()) != 2 {
					t.Fatalf("ordinary upstream status must retry free, got %d requests", attempts)
				}
			})
		}
	}
}

func TestFreeMimoTailDiagnosticOncePerExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-stream=%t", stream), func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
			e := geoDiagnosticExecutor(t, path, false, "chat")
			body := geoDiagnosticMimoTailSSE()
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, upstream pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				geoDiagnosticAssertMimoRequest(t, upstream)
				return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
			}}
			req := geoDiagnosticRequest(fake, "mimo-free")
			geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, req, stream), true)
			geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, req, stream), false)
			calls := fake.snapshot()
			if len(calls) != 2 {
				t.Fatalf("two logical failed requests should make one free call each and zero paid calls, got %d", len(calls))
			}
		})
	}
}

func TestFreeMimoTailDiagnosticDoesNotAnnotateOtherCases(t *testing.T) {
	cases := []struct {
		name, model, mimoEndpoint, path string
		body                            []byte
	}{
		{"other-chat-alias", "ling-free", "chat", "/v1/chat/completions", geoDiagnosticMimoTailSSE()},
		{"responses-alias", "muse-free", "chat", "/v1/responses", geoDiagnosticResponsesTailSSE()},
		{"mimo-alias-responses-endpoint", "mimo-free", "responses", "/v1/responses", geoDiagnosticResponsesTailSSE()},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
				e := geoDiagnosticExecutor(t, path, false, tc.mimoEndpoint)
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, upstream pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					if !strings.HasSuffix(upstream.URL, tc.path) {
						t.Fatal("non-Mimo fixture used wrong upstream endpoint")
					}
					return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: tc.body}, nil
				}}
				geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, geoDiagnosticRequest(fake, tc.model), stream), false)
				if got := len(fake.snapshot()); got != 1 {
					t.Fatalf("unannotated failure should use one free request, got %d", got)
				}
			})
		}
	}
}

func TestFreeMimoTailOtherAliasesDoNotConsumeDiagnostic(t *testing.T) {
	path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
	e := geoDiagnosticExecutor(t, path, false, "chat")
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, upstream pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(upstream.URL, "/v1/responses") {
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: geoDiagnosticResponsesTailSSE()}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: geoDiagnosticMimoTailSSE()}, nil
	}}
	geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, geoDiagnosticRequest(fake, "ling-free"), false), false)
	geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, geoDiagnosticRequest(fake, "muse-free"), false), false)
	mimo := geoDiagnosticRequest(fake, "mimo-free")
	geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, mimo, false), true)
	geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, mimo, false), false)
	if got := len(fake.snapshot()); got != 4 {
		t.Fatalf("unrelated aliases must leave the one-shot Mimo diagnostic for the first Mimo call; got %d free calls", got)
	}
}

func TestFreeMimoTailDiagnosticRequiresExactParseFailure(t *testing.T) {
	path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
	e := geoDiagnosticExecutor(t, path, false, "chat")
	// Terminated but without a finish_reason: this is not the observed
	// data-after-DONE failure, so it must not consume the one-shot diagnostic.
	invalid := freeFixSSE(`data: {"choices":[{"delta":{"content":"unfinished"}}]}`, `data: [DONE]`)
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, upstream pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		geoDiagnosticAssertMimoRequest(t, upstream)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: invalid}, nil
	}}
	req := geoDiagnosticRequest(fake, "mimo-free")
	err := availabilityRequestError(t, e, req, false)
	availabilityStatus(t, err, http.StatusBadGateway)
	if strings.Contains(err.Error(), geoDiagnosticTailMarker) {
		t.Fatal("non-tail parser failure consumed the Mimo diagnostic")
	}
	fake.answer = func(_ context.Context, upstream pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		geoDiagnosticAssertMimoRequest(t, upstream)
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: geoDiagnosticMimoTailSSE()}, nil
	}
	geoDiagnosticAssertSummary(t, availabilityRequestError(t, e, req, false), true)
	if got := len(fake.snapshot()); got != 2 {
		t.Fatalf("expected one free call per parser failure, got %d", got)
	}
}

func TestFreeMimoTailDiagnosticConcurrentOneShot(t *testing.T) {
	path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
	e := geoDiagnosticExecutor(t, path, false, "chat")
	body := geoDiagnosticMimoTailSSE()
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		// No testing.T calls in worker goroutines.
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: append([]byte(nil), body...)}, nil
	}}
	req := geoDiagnosticRequest(fake, "mimo-free")
	const workers = 8
	errorsOut := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Execute(context.Background(), req)
			errorsOut <- err
		}()
	}
	wg.Wait()
	close(errorsOut)
	observed := 0
	for err := range errorsOut {
		if err != nil && strings.Contains(err.Error(), geoDiagnosticTailMarker) {
			observed++
			geoDiagnosticAssertSummary(t, err, true)
		} else {
			geoDiagnosticAssertSummary(t, err, false)
		}
	}
	if observed != 1 || len(fake.snapshot()) != workers {
		t.Fatalf("one executor must emit one diagnostic for %d concurrent failures; emitted %d in %d calls", workers, observed, len(fake.snapshot()))
	}
}
