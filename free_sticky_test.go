package plugin

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func stickyExecutor(t *testing.T, poolPath string) *Executor {
	t.Helper()
	cfg, err := parseConfigStrict([]byte(fmt.Sprintf(`paid:
  api_keys:
    - key: sk-test
  base_url: https://example.invalid
free:
  enabled: true
  session_pool: %q
  members:
    - key: sticky-key-a
    - key: sticky-key-b
  models:
    - alias: muse-free
      name: syntheticResponsesEntry
      endpoint: responses
  quota:
    cooldown: 300
`, poolPath)))
	if err != nil {
		t.Fatalf("invalid sticky test config: %v", err)
	}
	return NewExecutor(cfg)
}

func stickyAuthKey(req pluginapi.HTTPRequest) string {
	return strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer ")
}

func TestFreeStickyMemberReusedFirst(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
			e := stickyExecutor(t, path)
			fake := &freeFixFakeHTTP{answer: func(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if !strings.HasSuffix(request.URL, "/v1/responses") {
					t.Fatal("sticky request left the responses endpoint")
				}
				return availabilityFreeSuccess(request), nil
			}}
			availabilityResponse(t, e, geoDiagnosticRequest(fake, "muse-free"), stream, "Recovered")
			first := stickyAuthKey(fake.snapshot()[0])
			if first != "sticky-key-a" && first != "sticky-key-b" {
				t.Fatalf("unexpected serving member: %q", first)
			}
			availabilityResponse(t, e, geoDiagnosticRequest(fake, "muse-free"), stream, "Recovered")
			got := fake.snapshot()
			if len(got) != 2 {
				t.Fatalf("steady requests must cost one upstream call each, got %d", len(got))
			}
			if stickyAuthKey(got[1]) != first {
				t.Fatalf("second request must reuse member %q first, got %q", first, stickyAuthKey(got[1]))
			}
		})
	}
}

func TestFreeStickyMemberSwitchesWhenCooled(t *testing.T) {
	path := availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0))
	e := stickyExecutor(t, path)
	order := []int{0, 1}
	if got := e.freepool.preferFirst("s", order); !reflect.DeepEqual(got, order) {
		t.Fatal("unknown session must keep random order")
	}
	e.freepool.remember("s", 1)
	if got := e.freepool.preferFirst("s", order); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("remembered member must move first, got %v", got)
	}
	if got := e.freepool.preferFirst("s", []int{0}); !reflect.DeepEqual(got, []int{0}) {
		t.Fatal("single-member order must stay untouched")
	}
	e.freepool.cool(1, 300)
	if got := e.freepool.preferFirst("s", order); !reflect.DeepEqual(got, order) {
		t.Fatalf("cooled member must lose stickiness, got %v", got)
	}
}

func TestFreeAttemptTimeoutFailsOverToNextMember(t *testing.T) {
	poolPath := availabilityPoolFile(t, availabilitySessions(t, 2, 0, 0))
	e := availabilityExecutor(poolPath, false)
	calls := 0
	fake := &freeFixFakeHTTP{answer: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/v1/responses") {
			t.Fatal("timeout failover must stay on the free endpoint")
		}
		calls++
		if calls == 1 {
			return pluginapi.HTTPResponse{}, &url.Error{Op: "Post", URL: req.URL, Err: context.DeadlineExceeded}
		}
		return availabilityFreeSuccess(req), nil
	}}
	if marker := availabilityResponse(t, e, freeFixRequest(fake), false, "Recovered").Get(fallbackHeader); marker != "" {
		t.Fatal("timeout failover must not use paid fallback")
	}
	if calls != 2 || len(fake.snapshot()) != 2 {
		t.Fatalf("attempt timeout must fail over once, got %d calls", calls)
	}
}

func TestFreeAttemptTimeoutDefaultsTo120(t *testing.T) {
	e := availabilityExecutor(availabilityPoolFile(t, availabilitySessions(t, 1, 0, 0)), false)
	if e.freepool.timeoutSec != 120 {
		t.Fatalf("default attempt timeout = %d, want 120", e.freepool.timeoutSec)
	}
	cfg, err := parseConfigStrict([]byte(`paid:
  api_keys:
    - key: sk-test
free:
  enabled: true
  members:
    - key: public
  quota:
    attempt_timeout: 45
`))
	if err != nil {
		t.Fatalf("invalid timeout test config: %v", err)
	}
	if got := NewExecutor(cfg).freepool.timeoutSec; got != 45 {
		t.Fatalf("custom attempt timeout = %d, want 45", got)
	}
}
