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
	failAt int
	writes int
}

func (s *memoryStore) Get(key string) (string, error) {
	value, ok := s.values[key]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}
func (s *memoryStore) Set(key, value string) error {
	s.writes++
	if s.fail || s.failAt == s.writes {
		return errors.New("credential backend unavailable")
	}
	s.values[key] = value
	return nil
}
func (s *memoryStore) Delete(key string) error { delete(s.values, key); return nil }

func newTestEditor(t *testing.T) *editor {
	t.Helper()
	e, err := openEditor(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.UpstreamURL = "https://example.com"
	e.store = &memoryStore{values: map[string]string{}}
	return e
}

func TestSaveStoresKeysAndReturnsRunnableConfiguration(t *testing.T) {
	e := newTestEditor(t)
	e.cfg.UpstreamAPIKeyRef, e.cfg.LocalAPIKeyRef = "pending", "pending"
	e.pendingUpstream, e.pendingLocal = "private-upstream", "private-local"
	result, err := e.save(true)
	if err != nil || !result.Start || !result.Saved {
		t.Fatalf("save and start: %+v %v", result, err)
	}
	secrets, err := result.Config.ResolveSecrets(e.store)
	if err != nil || secrets.UpstreamKey != "private-upstream" || secrets.LocalKey != "private-local" {
		t.Fatal("persisted keys unavailable")
	}
	data, _ := os.ReadFile(e.path)
	if strings.Contains(string(data), "private-") || e.dirty() {
		t.Fatal("key leaked into configuration or editor remains dirty")
	}
	loaded, err := config.LoadFile(e.path)
	if err != nil || loaded != result.Config {
		t.Fatal("saved configuration is not loadable")
	}
}

func TestCredentialFailureAndSaveFailureDoNotLeakNewCredentials(t *testing.T) {
	t.Run("credential failure", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKeyRef = "pending"
		e.pendingUpstream = "private-key"
		e.store.(*memoryStore).fail = true
		if _, err := e.save(false); err == nil {
			t.Fatal("credential failure ignored")
		}
		if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("configuration saved despite credential failure")
		}
	})
	t.Run("save failure", func(t *testing.T) {
		e := newTestEditor(t)
		if _, err := e.save(false); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(e.path+".bak", 0700); err != nil {
			t.Fatal(err)
		}
		e.cfg.UpstreamAPIKeyRef = "pending"
		e.pendingUpstream = "private-key"
		if _, err := e.save(false); err == nil {
			t.Fatal("file failure ignored")
		}
		if len(e.store.(*memoryStore).values) != 0 {
			t.Fatal("failed save leaked credentials")
		}
	})
	t.Run("second credential failure", func(t *testing.T) {
		e := newTestEditor(t)
		e.cfg.UpstreamAPIKeyRef, e.cfg.LocalAPIKeyRef = "pending", "pending"
		e.pendingUpstream, e.pendingLocal = "upstream-secret", "local-secret"
		e.store.(*memoryStore).failAt = 2
		if _, err := e.save(false); err == nil {
			t.Fatal("second credential failure ignored")
		}
		if len(e.store.(*memoryStore).values) != 0 {
			t.Fatal("partial credential transaction leaked")
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
	var env, reference, pending string
	if err := e.editKey("API Key", "沿用客户端认证", &env, &reference, &pending); err != nil {
		t.Fatal(err)
	}
	if pending != "private-new-key" || reference != "pending" || len(e.store.(*memoryStore).values) != 0 {
		t.Fatal("key entry was not deferred until save")
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
