package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Happy2018new/local-ai-relay/internal/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

type memoryStore struct {
	values map[string]string
	fail   bool
	reads  int
}

func (s *memoryStore) Get(key string) (string, error) {
	s.reads++
	if s.fail {
		return "", errors.New("credential backend unavailable")
	}
	value, ok := s.values[key]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}
func newTestEditor(t *testing.T) *editor {
	t.Helper()
	e, err := openEditor(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.UpstreamURL = "https://example.com"
	e.legacy = &memoryStore{values: map[string]string{}, fail: true}
	return e
}

func TestSaveWritesInlineKeysWithoutCredentialStore(t *testing.T) {
	e := newTestEditor(t)
	e.cfg.UpstreamAPIKey, e.cfg.LocalAPIKey = "private-upstream", "private-local"
	result, err := e.save(true)
	if err != nil || !result.Start || !result.Saved {
		t.Fatalf("save and start: %+v %v", result, err)
	}
	secrets, err := result.Config.ResolveSecrets(nil)
	if err != nil || secrets.UpstreamKey != "private-upstream" || secrets.LocalKey != "private-local" {
		t.Fatal("persisted keys unavailable")
	}
	data, _ := os.ReadFile(e.path)
	if !strings.Contains(string(data), `"upstream_api_key": "private-upstream"`) || !strings.Contains(string(data), `"local_api_key": "private-local"`) || e.dirty() {
		t.Fatal("keys were not saved in configuration or editor remains dirty")
	}
	if e.legacy.(*memoryStore).reads != 0 {
		t.Fatal("inline key configuration accessed system credentials")
	}
	loaded, err := config.LoadFile(e.path)
	if err != nil || loaded != result.Config {
		t.Fatal("saved configuration is not loadable")
	}
}

func TestLegacyMigrationAndSaveFailurePreservePreviousConfiguration(t *testing.T) {
	t.Run("unavailable legacy credential", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKeyRef = "legacy-upstream"
		if _, err := e.save(false); err == nil {
			t.Fatal("missing legacy credential ignored")
		}
		if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configuration saved despite credential failure")
		}
	})
	t.Run("save failure", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKey = "previous-key"
		if _, err := e.save(false); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(e.path+".bak", 0700); err != nil {
			t.Fatal(err)
		}
		e.cfg.UpstreamAPIKey = "replacement-key"
		if _, err := e.save(false); err == nil {
			t.Fatal("file failure ignored")
		}
		previous, err := config.LoadFile(e.path)
		if err != nil || previous.UpstreamAPIKey != "previous-key" || e.cfg.UpstreamAPIKey != "replacement-key" || !e.dirty() {
			t.Fatal("failed save discarded the draft or changed the saved configuration")
		}
	})
	t.Run("migrate both legacy keys", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKeyRef, e.cfg.LocalAPIKeyRef = "old-upstream", "old-local"
		e.legacy = &memoryStore{values: map[string]string{"old-upstream": "upstream-secret", "old-local": "local-secret"}}
		if err := config.Save(e.path, e.cfg); err != nil {
			t.Fatal(err)
		}
		e.original, e.exists = e.cfg, true
		result, err := e.save(true)
		if err != nil || result.Config.UpstreamAPIKey != "upstream-secret" || result.Config.LocalAPIKey != "local-secret" || result.Config.UpstreamAPIKeyRef != "" || result.Config.LocalAPIKeyRef != "" {
			t.Fatal("legacy configuration was not migrated")
		}
		if _, err := result.Config.ResolveSecrets(nil); err != nil {
			t.Fatal("migrated configuration still requires system credentials")
		}
		backup, err := config.LoadFile(e.path + ".bak")
		if err != nil || backup.UpstreamAPIKeyRef != "old-upstream" {
			t.Fatal("legacy configuration was not backed up")
		}
	})
	t.Run("incomplete legacy migration", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKeyRef, e.cfg.LocalAPIKeyRef = "old-upstream", "missing-local"
		e.legacy = &memoryStore{values: map[string]string{"old-upstream": "upstream-secret"}}
		before := e.cfg
		if _, err := e.save(false); err == nil {
			t.Fatal("missing second legacy credential ignored")
		}
		if e.cfg != before {
			t.Fatal("failed migration partially modified the draft")
		}
		if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial migration was written to disk")
		}
	})
}

func TestAPIKeyEntryIsMaskedAndDeferredUntilSave(t *testing.T) {
	e := newTestEditor(t)
	step := 0
	e.run = func(form *huh.Form) error {
		field := form.GetFocusedField()
		field.Focus()
		if step == 0 {
			field.Update(tea.KeyMsg{Type: tea.KeyDown})
		} else {
			field.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-new-key")})
			if strings.Contains(form.View(), "private-new-key") {
				t.Fatal("API key was visible in form")
			}
		}
		step++
		return nil
	}
	var key, env, reference string
	if err := e.editKey("API Key", "沿用客户端认证", &key, &env, &reference); err != nil {
		t.Fatal(err)
	}
	if key != "private-new-key" || reference != "" || e.legacy.(*memoryStore).reads != 0 {
		t.Fatal("key entry was not deferred until save")
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("key entry saved a configuration before Save was chosen")
	}
}

func TestAPIKeyCanBePreservedReplacedAndRemoved(t *testing.T) {
	for _, action := range []string{"preserve", "replace", "remove"} {
		t.Run(action, func(t *testing.T) {
			e := newTestEditor(t)
			key, env, reference := "previous-key", "", ""
			step := 0
			e.run = func(form *huh.Form) error {
				field := form.GetFocusedField()
				field.Focus()
				if step == 0 && action == "remove" {
					field.Update(tea.KeyMsg{Type: tea.KeyUp})
				} else if step == 1 && action == "replace" {
					field.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("replacement-key")})
				}
				step++
				return nil
			}
			if err := e.editKey("API Key", "沿用客户端认证", &key, &env, &reference); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"preserve": "previous-key", "replace": "replacement-key", "remove": ""}[action]
			if key != want || env != "" || reference != "" {
				t.Fatal("key edit did not apply the selected action")
			}
		})
	}
}

func TestExternalChangesAreNotOverwritten(t *testing.T) {
	e := newTestEditor(t)
	if _, err := e.save(false); err != nil {
		t.Fatal(err)
	}
	external := e.cfg
	external.MaxConcurrent = 3
	if err := config.Save(e.path, external); err != nil {
		t.Fatal(err)
	}
	e.cfg.MaxConcurrent = 2
	if _, err := e.save(false); err == nil {
		t.Fatal("external edit overwritten")
	}
	loaded, _ := config.LoadFile(e.path)
	if loaded.MaxConcurrent != 3 {
		t.Fatal("external edit lost")
	}
}

func TestCanceledSectionLeavesDraftUnchanged(t *testing.T) {
	e := newTestEditor(t)
	original := e.cfg
	e.run = func(form *huh.Form) error {
		field := form.GetFocusedField()
		field.Focus()
		field.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
		field.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("99")})
		return huh.ErrUserAborted
	}
	if err := e.edit("queue"); !errors.Is(err, huh.ErrUserAborted) {
		t.Fatalf("cancel: %v", err)
	}
	if e.cfg != original {
		t.Fatal("canceled section changed draft")
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled section saved configuration")
	}
}

func TestMenuFitsNarrowTerminal(t *testing.T) {
	e := newTestEditor(t)
	for _, width := range []int{32, 50, 100} {
		action := "connection"
		form := e.menu(&action)
		responsiveForm{form}.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		for _, line := range strings.Split(form.View(), "\n") {
			if lipgloss.Width(line) > width {
				t.Errorf("menu line wider than %d: %q", width, line)
			}
		}
	}
}
