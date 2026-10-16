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

var _ credentials.Reader = keyStore{}

func TestLegacyCredentialsMigrateToEditableConfigKeys(t *testing.T) {
	cfg := Default()
	cfg.UpstreamURL = "https://example.com"
	cfg.UpstreamAPIKeyRef, cfg.LocalAPIKeyRef = "upstream-test", "local-test"
	store := keyStore{"upstream-test": "upstream-secret", "local-test": "local-secret"}
	secrets, err := cfg.ResolveSecrets(store)
	if err != nil || secrets.UpstreamKey != "upstream-secret" || secrets.LocalKey != "local-secret" {
		t.Fatalf("secret resolution: %+v %v", secrets, err)
	}
	migrated, err := cfg.InlineLegacyKeys(store)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.UpstreamAPIKeyRef != "" || migrated.LocalAPIKeyRef != "" || cfg.UpstreamAPIKeyRef == "" {
		t.Fatal("migration did not clear references or modified its source")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, migrated); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"upstream_api_key": "upstream-secret"`) || !strings.Contains(string(data), `"local_api_key": "local-secret"`) || strings.Contains(string(data), "_api_key_ref") {
		t.Fatal("migrated keys were not saved in configuration")
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err = loaded.ResolveSecrets(nil)
	if err != nil || secrets.UpstreamKey != "upstream-secret" || secrets.LocalKey != "local-secret" {
		t.Fatal("migrated configuration still requires the credential store")
	}
}
