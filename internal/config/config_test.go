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
		`{"upstream_url":"https://example.com","listen":"127.0.0.1:70000"}`,
		`{"upstream_url":"https://example.com","listen":"127.0.0.1:abc"}`,
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
	cfg.UpstreamURL = "https://example.com"
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

func TestInlineAPIKeysAndMixedSources(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"upstream_url":"https://example.com/v1","upstream_api_key":"upstream-direct","local_api_key":"local-direct"}`))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := cfg.ResolveSecrets(nil)
	if err != nil || secrets.UpstreamKey != "upstream-direct" || secrets.LocalKey != "local-direct" {
		t.Fatal("inline keys did not resolve without a credential store")
	}
	cfg.LocalAPIKey = ""
	cfg.LocalAPIKeyEnv = "RELAY_TEST_LOCAL_KEY"
	t.Setenv(cfg.LocalAPIKeyEnv, "local-from-env")
	secrets, err = cfg.ResolveSecrets(nil)
	if err != nil || secrets.UpstreamKey != "upstream-direct" || secrets.LocalKey != "local-from-env" {
		t.Fatal("independent key sources did not resolve")
	}
	for _, data := range []string{
		`{"upstream_api_key":"private-key","upstream_api_key_env":"UPSTREAM"}`,
		`{"upstream_api_key":"private-key","upstream_api_key_ref":"old-reference"}`,
		`{"upstream_api_key_env":"UPSTREAM","upstream_api_key_ref":"old-reference"}`,
		`{"local_api_key":"private-key"}`,
		`{"upstream_api_key":"upstream","local_api_key":"private-key","local_api_key_env":"LOCAL"}`,
		`{"upstream_api_key":"upstream","local_api_key":"private-key","local_api_key_ref":"old-reference"}`,
		`{"upstream_api_key":"private-key\n"}`,
		`{"upstream_api_key":"private-key\u0000"}`,
		`{"upstream_api_key":"   "}`,
	} {
		_, err := Load(strings.NewReader(`{"upstream_url":"https://example.com",` + data[1:]))
		if err == nil {
			t.Error("invalid key configuration accepted")
		} else if strings.Contains(err.Error(), "private-key") {
			t.Error("API key included in validation error")
		}
	}
}
