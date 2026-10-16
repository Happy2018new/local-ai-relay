package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestTailLogIsBoundedAndHandlesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.log")
	if lines, err := tailLog(path); err != nil || len(lines) != 0 {
		t.Fatal("missing log is not empty")
	}
	var data strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&data, "%04d %s\n", i, strings.Repeat("x", 700))
	}
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(path)
	if err != nil || len(lines) != maxLogLines || !strings.HasPrefix(lines[len(lines)-1], "1999 ") {
		t.Fatalf("bounded tail: %d %v", len(lines), err)
	}
	if _, err := tailLog(filepath.Dir(path)); err == nil {
		t.Fatal("directory accepted as log")
	}
}

func TestLogFilteringSearchingAndTerminalWidths(t *testing.T) {
	m := newLogsModel("relay.log", "运行日志")
	m.lines = []string{
		`{"time":"2026-10-16T04:00:00Z","level":"INFO","msg":"relay_started"}`,
		`{"level":"WARN","msg":"upstream_attempt","status":429,"request_id":"relay-123"}`,
		`{"level":"ERROR","msg":"another_error","status":502}`,
	}
	m.refresh()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m = updated.(logsModel)
	if m.matched != 2 {
		t.Fatalf("warning filter matched %d", m.matched)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(logsModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("429")})
	m = updated.(logsModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(logsModel)
	if m.matched != 1 || m.query != "429" {
		t.Fatal("search did not combine with level filter")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = updated.(logsModel)
	if m.follow {
		t.Fatal("follow did not pause")
	}
	for _, width := range []int{24, 40, 80} {
		updated, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		m = updated.(logsModel)
		for _, line := range strings.Split(m.View(), "\n") {
			if lipgloss.Width(line) > width {
				t.Fatalf("log line wider than %d: %q", width, line)
			}
		}
	}
}

func TestLogContentCannotInjectTerminalEscapes(t *testing.T) {
	m := newLogsModel("relay.log", "logs")
	m.lines = []string{"bad\x1b[2Jline\x07"}
	m.refresh()
	if view := m.viewport.View(); strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x07") {
		t.Fatal("terminal control characters were rendered")
	}
}
