package plugin

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationNormalizesFreeEndpoints(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "chat"},
		{" CHAT ", "chat"},
		{" ReSPonses ", "responses"},
		{" SYStemOne ", "systemone"},
	} {
		t.Run(tc.want+"/"+tc.input, func(t *testing.T) {
			cfg, err := parseConfigStrict([]byte("free:\n  enabled: true\n  models:\n    - alias: test-free\n      name: upstream-free\n      endpoint: '" + tc.input + "'\n"))
			if err != nil {
				t.Fatalf("parse valid endpoint: %v", err)
			}
			if got := cfg.freeEntry("test-free"); got == nil || got.Endpoint != tc.want {
				t.Fatalf("registered endpoint = %+v; want %q", got, tc.want)
			}
			if cfg.Free.Models[0].Endpoint != tc.want {
				t.Fatalf("configured endpoint = %q; want %q", cfg.Free.Models[0].Endpoint, tc.want)
			}
		})
	}
	cfg, err := parseConfigStrict([]byte("free:\n  enabled: true\n"))
	if err != nil || cfg.freeEntry("muse-free").Endpoint != "responses" {
		t.Fatalf("built-in default endpoint changed: cfg=%+v error=%v", cfg, err)
	}
}

func TestRegistrationRejectsInvalidFreeEndpointWithoutSecrets(t *testing.T) {
	raw := []byte(`paid:
  api_keys:
    - key: sk-secret-test-key
      proxy_url: http://user:password@127.0.0.1:8080
free:
  enabled: true
  models:
    - alias: test-free
      name: upstream-free
      endpoint: resposes-secret
  members:
    - key: sk-secret-free
      proxy_url: socks5://user:password@127.0.0.1:1080
`)
	_, err := parseConfigStrict(raw)
	if err == nil || !strings.Contains(err.Error(), "free.models[0].endpoint") {
		t.Fatalf("invalid endpoint should fail with field path: %v", err)
	}
	for _, secret := range []string{"secret", "password", "http://", "socks5://", "resposes"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error leaked configured values: %v", err)
		}
	}
	if _, _, err := Build(raw); err == nil {
		t.Fatal("registration accepted invalid endpoint")
	}
}

func TestRegistrationRedactsMalformedYAMLValues(t *testing.T) {
	_, err := parseConfigStrict([]byte("paid:\n  api_keys:\n    - key: sk-secret-value\n      weight: sk-secret-value\n"))
	if err == nil || strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("malformed YAML exposed a credential value: %v", err)
	}
}

func TestRegistrationValidatesEffectivePaidWeightSum(t *testing.T) {
	tooLarge := []byte(fmt.Sprintf("paid:\n  api_keys:\n    - key: sk-secret-1\n      weight: %d\n    - key: sk-secret-2\n      weight: 1\n", math.MaxInt))
	_, err := parseConfigStrict(tooLarge)
	if err == nil || !strings.Contains(err.Error(), "paid.api_keys[1].weight") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("paid weight overflow: %v", err)
	}
	atLimit := []byte(fmt.Sprintf("paid:\n  api_keys:\n    - key: sk-a\n      weight: %d\n    - key: ' '\n      weight: %d\n", math.MaxInt, math.MaxInt))
	cfg, err := parseConfigStrict(atLimit)
	if err != nil {
		t.Fatalf("blank-key member is not part of paid pool: %v", err)
	}
	if got := cfg.members(pluginapi.ExecutorRequest{}); len(got) != 1 || got[0].normWeight() != math.MaxInt {
		t.Fatalf("effective paid members = %+v", got)
	}
}

func TestRegistrationValidatesEffectiveFreeWeightSum(t *testing.T) {
	tooLarge := []byte(fmt.Sprintf("free:\n  enabled: true\n  members:\n    - key: sk-secret-1\n      weight: %d\n    - key: public\n      weight: 1\n", math.MaxInt))
	_, err := parseConfigStrict(tooLarge)
	if err == nil || !strings.Contains(err.Error(), "free.members[1].weight") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("free weight overflow: %v", err)
	}
	atLimit := []byte(fmt.Sprintf("free:\n  enabled: true\n  members:\n    - key: sk-a\n      weight: %d\n    - key: sk-disabled\n      weight: %d\n      disabled: true\n", math.MaxInt, math.MaxInt))
	cfg, err := parseConfigStrict(atLimit)
	if err != nil || len(newFreePool(cfg).members) != 1 {
		t.Fatalf("disabled member is not effective: err=%v", err)
	}
}

func TestRegistrationPreservesDefaultWeights(t *testing.T) {
	cfg, err := parseConfigStrict([]byte("paid:\n  api_keys:\n    - key: sk-a\n      weight: 0\n    - key: sk-b\n      weight: -5\nfree:\n  enabled: true\n  members:\n    - key: public\n      weight: 0\n    - key: sk-c\n      weight: -2\n"))
	if err != nil {
		t.Fatalf("default weights should remain valid: %v", err)
	}
	for _, entry := range cfg.Paid.APIKeys {
		if entry.normWeight() != 1 {
			t.Fatalf("paid default weight changed: %+v", entry)
		}
	}
	for _, entry := range cfg.Free.Members {
		if entry.normWeight() != 1 {
			t.Fatalf("free default weight changed: %+v", entry)
		}
	}
}
