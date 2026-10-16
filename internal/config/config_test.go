package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"upstream_url":"https://example.com/v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrent != 1 || !cfg.RequireStream || cfg.MaxAttempts != 5 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	for _, data := range []string{
		`{"upstream_url":"https://example.com","listen":"0.0.0.0:8787"}`,
		`{"upstream_url":"https://key@example.com"}`,
		`{"upstream_url":"https://example.com?key=secret"}`,
		`{"upstream_url":"https://example.com","max_attempts":0}`,
		`{"upstream_url":"https://example.com","min_interval":"-1s"}`,
		`{"upstream_url":"https://example.com","queue_timeout":2}`,
		`{"upstream_url":"https://example.com","typo":1}`,
		`{"upstream_url":"https://example.com","local_api_key_env":"LOCAL"}`,
		`{"upstream_url":"https://example.com"} {}`,
	} {
		if _, err := Load(strings.NewReader(data)); err == nil {
			t.Errorf("accepted invalid configuration: %s", data)
		}
	}
}

func TestSecretsFailClosed(t *testing.T) {
	cfg := Default()
	cfg.UpstreamAPIKeyEnv = "RELAY_TEST_UPSTREAM_KEY"
	t.Setenv(cfg.UpstreamAPIKeyEnv, "")
	if _, err := cfg.Secrets(); err == nil {
		t.Fatal("missing configured key accepted")
	}
	t.Setenv(cfg.UpstreamAPIKeyEnv, "private-key")
	secrets, err := cfg.Secrets()
	if err != nil || secrets.UpstreamKey != "private-key" {
		t.Fatalf("secrets: %+v, %v", secrets, err)
	}
}
