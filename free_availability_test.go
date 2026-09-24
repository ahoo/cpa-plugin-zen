package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const availabilityGate = "OpenCode's free tier can only be used from within OpenCode"

var availabilityRetryField = regexp.MustCompile(`earliest_local_retry_at=([^\s,)]+)`)

func availabilityPoolFile(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func availabilitySessions(t *testing.T, count, fails int, cooldownUntil int64, earliest ...int64) []byte {
	t.Helper()
	entries := make([]map[string]any, count)
	for i := range entries {
		until := cooldownUntil
		if len(earliest) > 0 && i == count/2 {
			until = earliest[0]
		}
		entries[i] = map[string]any{
			"id": fmt.Sprintf("test-session-%02d", i), "fails": fails, "cooldown_until": until,
		}
	}
	raw, err := json.Marshal(map[string]any{"sessions": entries})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func availabilityExecutor(path string, fallback bool) *Executor {
	cfg := freeFixExecutor().cfg
	cfg.Free.SessionPool = path
	cfg.Free.Quota.FailoverPaid = fallback
	cfg.Free.Members = []FreeMember{{Key: "public"}, {Key: "public"}}
	return NewExecutor(cfg)
}

func availabilityStatus(t *testing.T, err error, want int) {
	t.Helper()
	var withStatus interface{ StatusCode() int }
	if !errors.As(err, &withStatus) {
		t.Fatalf("expected HTTP status %d, got error type %T", want, err)
	}
	if got := withStatus.StatusCode(); got != want {
		t.Fatalf("HTTP status = %d, want %d", got, want)
	}
}

func availabilityRequestError(t *testing.T, e *Executor, req pluginapi.ExecutorRequest, stream bool) error {
	t.Helper()
	if stream {
		resp, err := e.ExecuteStream(context.Background(), req)
		if resp.Chunks != nil {
			for ch := range resp.Chunks {
				if ch.Err == nil {
					t.Fatal("unavailable free request emitted a successful stream chunk")
				}
			}
		}
		if err == nil {
			t.Fatal("unavailable free request must return an immediate error")
		}
		if resp.Headers.Get("Retry-After") != "" {
			t.Fatal("retry hint must not be a response header")
		}
		return err
	}
	resp, err := e.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("unavailable free request returned a successful completion")
	}
	if resp.Headers.Get("Retry-After") != "" {
		t.Fatal("retry hint must not be a response header")
	}
	return err
}

func availabilityResponse(t *testing.T, e *Executor, req pluginapi.ExecutorRequest, stream bool, wantText string) http.Header {
	t.Helper()
	if stream {
		resp, err := e.ExecuteStream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream request failed: %v", err)
		}
		var content []byte
		for ch := range resp.Chunks {
			if ch.Err != nil {
				t.Fatalf("stream chunk failed: %v", ch.Err)
			}
			content = append(content, ch.Payload...)
		}
		if !bytes.Contains(content, []byte(wantText)) {
			t.Fatal("stream lost the successful completion")
		}
		return resp.Headers
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if !bytes.Contains(resp.Payload, []byte(wantText)) {
		t.Fatal("response lost the successful completion")
	}
	return resp.Headers
}

func availabilityFreeSuccess(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
	if strings.HasSuffix(req.URL, "/v1/systemone") {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"answers":{"x":{"type":"noul","noul":1}}}`)}
	}
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: freeFixSSE(
		`data: {"type":"response.output_text.done","item_id":"out","text":"Recovered"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	)}
}

func availabilityPaidSuccess() pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"Paid"},"finish_reason":"stop"}]}`)}
}

type availabilityDiskState struct {
	body    []byte
	modTime time.Time
}

func availabilitySnapshotFile(t *testing.T, path string) availabilityDiskState {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return availabilityDiskState{body: body, modTime: info.ModTime()}
}

func availabilityFileUnchanged(t *testing.T, path string, before availabilityDiskState) {
	t.Helper()
	after := availabilitySnapshotFile(t, path)
	if !bytes.Equal(before.body, after.body) || !before.modTime.Equal(after.modTime) {
		t.Fatal("unavailable request rewrote the session pool")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unavailable request left a session-pool temporary file")
	}
}

type availabilityMemberHealth struct {
	fails int
	until time.Time
}

func availabilityHealth(e *Executor) ([]sessionEntry, []availabilityMemberHealth) {
	e.sessions.mu.Lock()
	sessions := append([]sessionEntry(nil), e.sessions.sessions...)
	e.sessions.mu.Unlock()
	e.freepool.mu.Lock()
	members := make([]availabilityMemberHealth, len(e.freepool.members))
	for i, m := range e.freepool.members {
		members[i] = availabilityMemberHealth{fails: m.fails, until: m.cooldownUntil}
	}
	e.freepool.mu.Unlock()
	return sessions, members
}

func availabilityHealthUnchanged(t *testing.T, e *Executor, sessions []sessionEntry, members []availabilityMemberHealth) {
	t.Helper()
	gotSessions, gotMembers := availabilityHealth(e)
	if !reflect.DeepEqual(gotSessions, sessions) || !reflect.DeepEqual(gotMembers, members) {
		t.Fatal("unavailable request reported or cooled a session/member")
	}
}

func availabilityExpectRetry(t *testing.T, err error, want time.Time) {
	t.Helper()
	match := availabilityRetryField.FindStringSubmatch(err.Error())
	if len(match) != 2 || match[1] == "unknown" {
		t.Fatal("known cooldown must expose earliest_local_retry_at in error message")
	}
	if !strings.HasSuffix(match[1], "Z") {
		t.Fatal("earliest local retry must be UTC")
	}
	got, parseErr := time.Parse(time.RFC3339, match[1])
	if parseErr != nil {
		t.Fatalf("invalid local retry timestamp: %v", parseErr)
	}
	if delta := got.Sub(want); delta < -3*time.Second || delta > 3*time.Second {
		t.Fatalf("earliest local retry differs from eligible session/member time by %s", delta)
	}
}

func availabilityExpectUnknownRetry(t *testing.T, err error) {
	t.Helper()
	match := availabilityRetryField.FindStringSubmatch(err.Error())
	if len(match) != 2 || match[1] != "unknown" {
		t.Fatal("unrecoverable/unknown availability must not invent a local retry time")
	}
}

func TestFreeAvailabilityPolicy403FailsFastWithoutCooling(t *testing.T) {
	// JSON bodies below are synthetic fixtures, not recorded upstream payloads.
	gateBodies := []struct {
		name, body string
	}{
		{"plain-console", "Error from provider (Console): " + availabilityGate},
		{"plain-no-console", availabilityGate},
		{"json-error-message", `{"error":{"message":"Error from provider (Console): ` + availabilityGate + `"}}`},
		{"mixed-case-busy", `{"error":{"message":"opencode'S FREE TIER CAN ONLY BE USED FROM WITHIN opencode; backend busy, try again"}}`},
	}
	for _, tc := range gateBodies {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				poolPath := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
				e := availabilityExecutor(poolPath, false)
				beforeFile := availabilitySnapshotFile(t, poolPath)
				beforeSessions, beforeMembers := availabilityHealth(e)
				fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					if !strings.HasSuffix(req.URL, "/v1/responses") {
						t.Error("policy 403 dispatched a paid or wrong-endpoint request")
					}
					return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(tc.body)}, nil
				}}
				err := availabilityRequestError(t, e, freeFixRequest(fake), stream)
				availabilityStatus(t, err, http.StatusForbidden)
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(availabilityGate)) {
					t.Fatal("original gate message must be returned with status 403")
				}
				calls := fake.snapshot()
				if len(calls) != 1 || !strings.HasSuffix(calls[0].URL, "/v1/responses") || calls[0].Headers.Get("X-Opencode-Session") == "" {
					t.Fatalf("policy 403 must stop after one session/member free call, got %d calls", len(calls))
				}
				availabilityHealthUnchanged(t, e, beforeSessions, beforeMembers)
				availabilityFileUnchanged(t, poolPath, beforeFile)
			})
		}
	}
}

func TestFreeAvailabilityPolicy403ExplicitPaidFallbackOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			poolPath := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
			e := availabilityExecutor(poolPath, true)
			beforeFile := availabilitySnapshotFile(t, poolPath)
			beforeSessions, beforeMembers := availabilityHealth(e)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if strings.HasSuffix(req.URL, "/v1/responses") {
					return pluginapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(`{"error":{"message":"` + availabilityGate + `"}}`)}, nil
				}
				if !strings.HasSuffix(req.URL, "/v1/chat/completions") {
					t.Fatal("unexpected upstream route")
				}
				return availabilityPaidSuccess(), nil
			}}
			if got := availabilityResponse(t, e, freeFixRequest(fake), stream, "Paid").Get(fallbackHeader); got != "paid" {
				t.Fatal("explicit paid fallback must mark its response")
			}
			calls := fake.snapshot()
			if len(calls) != 2 || !strings.HasSuffix(calls[0].URL, "/v1/responses") || !strings.HasSuffix(calls[1].URL, "/v1/chat/completions") {
				t.Fatalf("policy 403 must make one free call and one paid resolver call, got %d", len(calls))
			}
			availabilityHealthUnchanged(t, e, beforeSessions, beforeMembers)
			availabilityFileUnchanged(t, poolPath, beforeFile)
		})
	}
}

func TestFreeAvailabilityNonPolicyStatusesStillRetry(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unrecognized-403", http.StatusForbidden, `{"error":{"message":"FreeTierError: OpenCode is busy, try again"}}`},
		{"partial-403", http.StatusForbidden, `{"error":{"message":"OpenCode free tier only, backend busy"}}`},
		{"same-gate-429", http.StatusTooManyRequests, `{"error":{"message":"` + availabilityGate + `"}}`},
		{"same-gate-503", http.StatusServiceUnavailable, "Error from provider (Console): " + availabilityGate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolPath := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
			e := availabilityExecutor(poolPath, false)
			attempts := 0
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(req.URL, "/v1/responses") {
					t.Fatal("status failover must not dispatch paid request")
				}
				attempts++
				if attempts == 1 {
					return pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}, nil
				}
				return availabilityFreeSuccess(req), nil
			}}
			if got := availabilityResponse(t, e, freeFixRequest(fake), false, "Recovered").Get(fallbackHeader); got != "" {
				t.Fatal("retrying an ordinary upstream error should stay on the free tier")
			}
			if attempts != 2 {
				t.Fatalf("ordinary upstream failure must rotate to next session/member, got %d calls", attempts)
			}
		})
	}
}

func TestFreeAvailabilityExplicitSessionPoolFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "empty-file", "empty-document", "bad-json", "unreadable"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "sessions.json")
				switch mode {
				case "empty-file":
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				case "empty-document":
					if err := os.WriteFile(path, []byte(`{"sessions":[]}`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "bad-json":
					if err := os.WriteFile(path, []byte(`{"sessions":`), 0o600); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					// A directory is reliably unreadable as a pool file even as root.
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				e := availabilityExecutor(path, false)
				fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
					t.Fatal("unavailable explicit pool must not send free or paid requests")
					return pluginapi.HTTPResponse{}, nil
				}}
				var before availabilityDiskState
				if mode != "missing" && mode != "unreadable" {
					before = availabilitySnapshotFile(t, path)
				}
				err := availabilityRequestError(t, e, freeFixRequest(fake), stream)
				availabilityStatus(t, err, http.StatusServiceUnavailable)
				availabilityExpectUnknownRetry(t, err)
				if len(fake.snapshot()) != 0 {
					t.Fatal("explicit pool failure sent an upstream request")
				}
				if mode == "missing" {
					if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("missing explicit pool must not be created by request")
					}
				} else if mode != "unreadable" {
					availabilityFileUnchanged(t, path, before)
				}
			})
		}
	}
}

func TestFreeAvailabilityDefaultPoolStates(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "bad-json", "valid"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir()) // Isolate the implicit plugins/zen-sessions.json path.
			path := filepath.Join("plugins", "zen-sessions.json")
			if mode != "missing" {
				if err := os.MkdirAll("plugins", 0o700); err != nil {
					t.Fatal(err)
				}
				var raw []byte
				switch mode {
				case "empty":
					raw = []byte(`{"sessions":[]}`)
				case "bad-json":
					raw = []byte(`not json`)
				case "valid":
					raw = availabilitySessions(t, 1, 0, 0)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e := availabilityExecutor("", false)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(req.URL, "/v1/responses") {
					t.Fatal("unexpected paid request")
				}
				return availabilityFreeSuccess(req), nil
			}}
			if mode == "empty" || mode == "bad-json" {
				before := availabilitySnapshotFile(t, path)
				err := availabilityRequestError(t, e, freeFixRequest(fake), false)
				availabilityStatus(t, err, http.StatusServiceUnavailable)
				availabilityExpectUnknownRetry(t, err)
				if len(fake.snapshot()) != 0 {
					t.Fatal("existing unusable default pool must not attempt sessionless traffic")
				}
				availabilityFileUnchanged(t, path, before)
				return
			}
			headers := availabilityResponse(t, e, freeFixRequest(fake), false, "Recovered")
			calls := fake.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected exactly one free call, got %d", len(calls))
			}
			if mode == "missing" {
				if calls[0].Headers.Get("X-Opencode-Session") != "" || headers.Get(poolSessionHeader) != "" {
					t.Fatal("missing implicit pool should preserve sessionless free traffic")
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("sessionless request must not mint a default pool file")
				}
			} else if calls[0].Headers.Get("X-Opencode-Session") != "test-session-00" || headers.Get(poolSessionHeader) != "test-session-00" {
				t.Fatal("existing valid default pool should automatically serve with its session")
			}
		})
	}
}

func TestFreeAvailabilitySystemOneIgnoresUnavailableSessionPool(t *testing.T) {
	paths := []struct {
		name string
		raw  []byte
	}{
		{"explicit-invalid", []byte(`not json`)},
		{"explicit-all-cool", availabilitySessions(t, 11, 1, time.Now().Add(time.Hour).Unix())},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			path := availabilityPoolFile(t, tc.raw)
			e := availabilityExecutor(path, false)
			before := availabilitySnapshotFile(t, path)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(req.URL, "/v1/systemone") || req.Headers.Get("X-Opencode-Session") != "" {
					t.Fatal("SystemOne must remain sessionless when pool is unavailable")
				}
				return availabilityFreeSuccess(req), nil
			}}
			req := freeFixRequest(fake)
			req.Model = "jev-free"
			req.Payload = []byte(`{"model":"jev-free","messages":[{"role":"user","content":"{\"state\":\"hi\",\"questions\":{\"x\":{\"type\":\"noul\",\"instructions\":\"x\"}}}"}]}`)
			if got := availabilityResponse(t, e, req, false, "noul").Get(fallbackHeader); got != "" {
				t.Fatal("SystemOne must not use paid fallback for unavailable session pool")
			}
			if len(fake.snapshot()) != 1 {
				t.Fatal("SystemOne sessionless call must reach upstream exactly once")
			}
			availabilityFileUnchanged(t, path, before)
		})
	}
}

func TestFreeAvailabilityStickyHealthySessionWins(t *testing.T) {
	for _, model := range []string{"muse-free", "jev-free"} {
		t.Run(model, func(t *testing.T) {
			poolPath := availabilityPoolFile(t, availabilitySessions(t, 3, 0, 0))
			e := availabilityExecutor(poolPath, false)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if req.Headers.Get("X-Opencode-Session") != "test-session-02" {
					t.Fatal("healthy stamped session lost priority to round-robin/sessionless selection")
				}
				return availabilityFreeSuccess(req), nil
			}}
			req := freeFixRequest(fake)
			req.Headers = http.Header{poolSessionHeader: {"test-session-02"}}
			wantText := "Recovered"
			if model == "jev-free" {
				req.Model = model
				req.Payload = []byte(`{"model":"jev-free","messages":[{"role":"user","content":"{\"state\":\"hi\",\"questions\":{\"x\":{\"type\":\"noul\",\"instructions\":\"x\"}}}"}]}`)
				wantText = "noul"
			}
			if got := availabilityResponse(t, e, req, false, wantText).Get(poolSessionHeader); got != "test-session-02" {
				t.Fatal("successful response must echo the healthy sticky pool assignment")
			}
			if len(fake.snapshot()) != 1 {
				t.Fatal("healthy sticky assignment must use only one upstream call")
			}
		})
	}
}

func TestFreeAvailabilityAllLocalCooldownsAndRetirement(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name            string
		sessions        []byte
		memberWait      []time.Duration
		earliest        time.Time
		unknownEarliest bool
	}{
		{"eleven-cooled-sessions", availabilitySessions(t, 11, 1, now.Add(49*time.Second).Unix(), now.Add(29*time.Second).Unix()), nil, now.Add(29 * time.Second), false},
		{"retired-sessions", availabilitySessions(t, 11, 5, now.Add(-time.Hour).Unix()), nil, time.Time{}, true},
		{"all-members-cooled", availabilitySessions(t, 1, 0, 0), []time.Duration{17 * time.Second, 42 * time.Second}, now.Add(17 * time.Second), false},
		{"session-and-members-cooled", availabilitySessions(t, 11, 1, now.Add(72*time.Second).Unix(), now.Add(52*time.Second).Unix()), []time.Duration{13 * time.Second, 36 * time.Second}, now.Add(52 * time.Second), false},
		{"retired-plus-members-cooled", availabilitySessions(t, 11, 5, now.Add(-time.Hour).Unix()), []time.Duration{13 * time.Second, 36 * time.Second}, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := availabilityPoolFile(t, tc.sessions)
			e := availabilityExecutor(path, false)
			if tc.memberWait != nil {
				e.freepool.mu.Lock()
				for i, duration := range tc.memberWait {
					e.freepool.members[i].cooldownUntil = now.Add(duration)
				}
				e.freepool.mu.Unlock()
			}
			before := availabilitySnapshotFile(t, path)
			beforeSessions, beforeMembers := availabilityHealth(e)
			fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				t.Fatal("local exhaustion must not send a free or paid request")
				return pluginapi.HTTPResponse{}, nil
			}}
			err := availabilityRequestError(t, e, freeFixRequest(fake), true)
			availabilityStatus(t, err, http.StatusServiceUnavailable)
			if tc.unknownEarliest {
				availabilityExpectUnknownRetry(t, err)
			} else {
				availabilityExpectRetry(t, err, tc.earliest)
			}
			if len(fake.snapshot()) != 0 {
				t.Fatal("local exhaustion dispatched upstream requests")
			}
			availabilityHealthUnchanged(t, e, beforeSessions, beforeMembers)
			availabilityFileUnchanged(t, path, before)
		})
	}
}

func TestFreeAvailabilityCooldownExpiresButRetiredSessionsDoNot(t *testing.T) {
	cases := []struct {
		name             string
		fails            int
		memberPreviously bool
		wantStatus       int
	}{
		{"session-expired", 4, false, http.StatusOK},
		{"member-expired", 0, true, http.StatusOK},
		{"retired-expired", 5, false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 1, tc.fails, time.Now().Add(-time.Minute).Unix()))
			e := availabilityExecutor(path, false)
			if tc.memberPreviously {
				e.freepool.mu.Lock()
				for i := range e.freepool.members {
					e.freepool.members[i].fails = 1
					e.freepool.members[i].cooldownUntil = time.Now().Add(-time.Minute)
				}
				e.freepool.mu.Unlock()
			}
			before := availabilitySnapshotFile(t, path)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return availabilityFreeSuccess(req), nil
			}}
			if tc.wantStatus == http.StatusServiceUnavailable {
				err := availabilityRequestError(t, e, freeFixRequest(fake), false)
				availabilityStatus(t, err, http.StatusServiceUnavailable)
				availabilityExpectUnknownRetry(t, err)
				if len(fake.snapshot()) != 0 {
					t.Fatal("expired cooldown must not resurrect retired sessions")
				}
				availabilityFileUnchanged(t, path, before)
				return
			}
			availabilityResponse(t, e, freeFixRequest(fake), false, "Recovered")
			calls := fake.snapshot()
			if len(calls) != 1 || calls[0].Headers.Get("X-Opencode-Session") != "test-session-00" {
				t.Fatal("naturally expired session/member must become usable again")
			}
		})
	}
}

func TestFreeAvailabilityExplicitPaidFallbackOnLocalFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 11, 5, time.Now().Add(-time.Minute).Unix()))
			e := availabilityExecutor(path, true)
			e.cfg.Paid.APIKeys = []APIKeyEntry{{Key: "sk-test-one"}, {Key: "sk-test-two"}}
			before := availabilitySnapshotFile(t, path)
			paidAttempts := 0
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(req.URL, "/v1/chat/completions") || req.Method != http.MethodPost {
					t.Fatal("unavailable local pool must only dispatch paid fallback calls")
				}
				var body map[string]any
				if err := json.Unmarshal(req.Body, &body); err != nil || body["model"] != "paid-model" || body["stream"] != false || body["stream_options"] != nil {
					t.Fatal("paid fallback must be a non-streaming request for the configured model")
				}
				paidAttempts++
				if paidAttempts == 1 {
					return pluginapi.HTTPResponse{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":"paid busy"}`)}, nil
				}
				return availabilityPaidSuccess(), nil
			}}
			if got := availabilityResponse(t, e, freeFixRequest(fake), stream, "Paid").Get(fallbackHeader); got != "paid" {
				t.Fatal("explicit fallback must mark paid output")
			}
			if paidAttempts != 2 || len(fake.snapshot()) != 2 {
				t.Fatalf("one resolver paid fallback sequence must keep paid-key failover, got %d paid calls", paidAttempts)
			}
			availabilityFileUnchanged(t, path, before)
		})
	}
}

func TestFreeAvailabilityCancellationPrecedesLocalFallback(t *testing.T) {
	path := availabilityPoolFile(t, availabilitySessions(t, 11, 5, time.Now().Add(-time.Hour).Unix()))
	e := availabilityExecutor(path, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &freeFixFakeHTTP{answer: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("canceled request must not hit free or paid upstream")
		return pluginapi.HTTPResponse{}, nil
	}}
	for _, stream := range []bool{false, true} {
		req := freeFixRequest(fake)
		if stream {
			_, err := e.ExecuteStream(ctx, req)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation must win over paid fallback, got %v", err)
			}
		} else {
			_, err := e.Execute(ctx, req)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation must win over paid fallback, got %v", err)
			}
		}
	}
	if len(fake.snapshot()) != 0 {
		t.Fatal("canceled request dispatched upstream calls")
	}
}
