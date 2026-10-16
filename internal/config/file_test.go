package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Happy2018new/local-ai-relay/internal/credentials"
)

func TestSaveRoundTripBackupAndInvalidEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := Default()
	cfg.UpstreamURL = "https://example.com/v1"
	cfg.LogFile = ".runtime/relay.log"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil || loaded != cfg {
		t.Fatalf("config round trip: %+v %v", loaded, err)
	}
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), `"queue_timeout": "3m0s"`) {
		t.Fatal("durations were not saved as strings")
	}
	cfg.MaxConcurrent = 2
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != string(before) {
		t.Fatal("previous configuration was not backed up")
	}
	cfg.MaxConcurrent = 0
	if err := Save(path, cfg); err == nil {
		t.Fatal("invalid configuration saved")
	}
	loaded, err = LoadFile(path)
	if err != nil || loaded.MaxConcurrent != 2 {
		t.Fatal("failed save modified existing configuration")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".local-ai-relay-*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files leaked: %v", leftovers)
	}
	if loaded.LogPath(path) != filepath.Join(filepath.Dir(path), ".runtime", "relay.log") {
		t.Fatal("relative log path was not based on configuration directory")
	}
}

type keyStore map[string]string

func (s keyStore) Get(reference string) (string, error) {
	value, ok := s[reference]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}
func (s keyStore) Set(reference, value string) error { s[reference] = value; return nil }
func (s keyStore) Delete(reference string) error     { delete(s, reference); return nil }

var _ credentials.Store = keyStore{}

func TestCredentialReferencesResolveWithoutPlaintextInJSON(t *testing.T) {
	cfg := Default()
	cfg.UpstreamURL = "https://example.com"
	cfg.UpstreamAPIKeyRef, cfg.LocalAPIKeyRef = "upstream-test", "local-test"
	store := keyStore{"upstream-test": "upstream-secret", "local-test": "local-secret"}
	secrets, err := cfg.ResolveSecrets(store)
	if err != nil || secrets.UpstreamKey != "upstream-secret" || secrets.LocalKey != "local-secret" {
		t.Fatalf("secret resolution: %+v %v", secrets, err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "upstream-secret") || strings.Contains(string(data), "local-secret") {
		t.Fatal("plaintext secret persisted")
	}
	cfg.UpstreamAPIKeyEnv = "UPSTREAM_KEY"
	if cfg.Validate() == nil {
		t.Fatal("conflicting key sources accepted")
	}
	cfg.UpstreamAPIKeyEnv, cfg.UpstreamAPIKeyRef = "", ""
	if cfg.Validate() == nil {
		t.Fatal("local key accepted without upstream key")
	}
}
