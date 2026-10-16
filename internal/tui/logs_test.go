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
	if lines, err := tailLog(path, ""); err != nil || len(lines) != 0 {
		t.Fatal("missing log is not empty")
	}
	var data strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&data, "%04d %s\n", i, strings.Repeat("x", 700))
	}
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(path, "")
	if err != nil || len(lines) != maxLogLines || !strings.HasPrefix(lines[len(lines)-1], "1999 ") {
		t.Fatalf("bounded tail: %d %v", len(lines), err)
	}
	if _, err := tailLog(filepath.Dir(path), ""); err == nil {
		t.Fatal("directory accepted as log")
	}
}

func TestLogFilteringSearchingAndTerminalWidths(t *testing.T) {
	m := newLogsModel("relay.log", "运行日志", "")
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
	m := newLogsModel("relay.log", "logs", "")
	m.lines = []string{"bad\x1b[2Jline\x07"}
	m.refresh()
	if view := m.viewport.View(); strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x07") {
		t.Fatal("terminal control characters were rendered")
	}
}

func TestSessionLogsExcludeHistoryAndIncompleteRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.log")
	data := `{"session_id":"old","msg":"relay_started"}` + "\n" + `{"session_id":"current","msg":"request_received"}` + "\n" + `{"session_id":"old","msg":"request_finished"}` + "\n" + `{"session_id":"current"`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(path, "current")
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "request_received") {
		t.Fatal("history or a partial record appeared in current session")
	}
}

func TestTableRowsAreSingleLineSelectableAndExpandable(t *testing.T) {
	m := newLogsModel("relay.log", "运行日志", "")
	m.lines = []string{`{"level":"WARN","msg":"stream_aborted","request_id":"relay-a","reason":"` + strings.Repeat("上游连接异常", 40) + `","reason_code":"unexpected_eof"}`, `{"level":"INFO","msg":"request_finished","status":200,"outcome":"completed","request_id":"relay-b"}`}
	m.refresh()
	if m.viewport.TotalLineCount() != 2 {
		t.Fatal("collapsed records wrapped into multiple lines")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(logsModel)
	if m.selected != 0 || m.follow {
		t.Fatal("selection did not pause follow")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(logsModel)
	if !m.expanded[m.entries[0].raw] || m.viewport.TotalLineCount() <= 2 {
		t.Fatal("record did not expand")
	}
	if !strings.Contains(m.viewport.View(), "上游连接异常") {
		t.Fatal("Chinese error detail missing")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(logsModel)
	if m.viewport.TotalLineCount() != 2 {
		t.Fatal("record did not collapse")
	}
	for _, width := range []int{1, 24, 40, 80, 160} {
		updated, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = updated.(logsModel)
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
		m = updated.(logsModel)
		for _, line := range strings.Split(m.View(), "\n") {
			if lipgloss.Width(line) > width {
				t.Fatalf("table line wider than %d", width)
			}
		}
	}
}

func TestTokenDisplayDoesNotDoubleCountCacheOrInventMissingCounts(t *testing.T) {
	d := map[string]any{"usage": map[string]any{"input_tokens": float64(39231), "output_tokens": float64(163), "total_tokens": float64(39394), "input_tokens_details": map[string]any{"cached_tokens": float64(27776)}}}
	input, output, read, write, total := usageValues(d)
	if input != "11,455" || output != "163" || read != "27,776" || write != "未报告" || total != "39,394" {
		t.Fatal("cache counts were duplicated or unknown usage became zero")
	}
}
