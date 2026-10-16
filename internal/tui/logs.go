package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const maxLogBytes = 1 << 20
const maxLogLines = 500

type logMessage struct {
	lines []string
	err   error
}
type logTick struct{}
type logEntry struct {
	raw  string
	data map[string]any
}

type logsModel struct {
	path, title, sessionID        string
	width, height                 int
	viewport                      viewport.Model
	search                        textinput.Model
	searching                     bool
	query                         string
	follow, warnings, raw         bool
	lines                         []string
	entries                       []logEntry
	err                           error
	matched, selected, horizontal int
	expanded                      map[string]bool
	selectedRaw                   string
	manualScroll                  bool
}

func ViewLogs(ctx context.Context, path, title, sessionID string) error {
	if path == "" {
		return errors.New("尚未设置日志文件")
	}
	_, err := tea.NewProgram(newLogsModel(path, title, sessionID), tea.WithOutput(os.Stderr), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func newLogsModel(path, title, sessionID string) logsModel {
	search := textinput.New()
	search.Prompt, search.CharLimit = "搜索  ", 160
	return logsModel{path: path, title: title, sessionID: sessionID, width: 80, height: 24,
		viewport: viewport.New(80, 18), search: search, follow: true, expanded: map[string]bool{}}
}

func (m logsModel) Init() tea.Cmd { return readLogs(m.path, m.sessionID) }
func readLogs(path, sessionID string) tea.Cmd {
	return func() tea.Msg { lines, err := tailLog(path, sessionID); return logMessage{lines, err} }
}

func tailLog(path, sessionID string) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("日志路径不是普通文件")
	}
	start := max(int64(0), info.Size()-maxLogBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLogBytes))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			return nil, nil
		}
	}
	// Ignore a concurrent append's incomplete final JSON record.
	if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
		data = data[:end]
	} else {
		return nil, nil
	}
	var lines []string
	for _, raw := range strings.Split(strings.ToValidUTF8(string(data), "?"), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if sessionID != "" {
			var record struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(raw), &record) != nil || record.SessionID != sessionID {
				continue
			}
		}
		lines = append(lines, raw)
	}
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	return lines, nil
}

func (m logsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 1), max(msg.Height, 1)
		m.viewport.Width, m.viewport.Height = m.width, max(m.height-6, 1)
		m.search.Width = max(m.width-8, 1)
		m.refresh()
	case logMessage:
		m.lines, m.err = msg.lines, msg.err
		m.refresh()
		return m, tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return logTick{} })
	case logTick:
		return m, readLogs(m.path, m.sessionID)
	case tea.MouseMsg:
		if msg.Button == tea.MouseButtonWheelUp {
			m.move(-3)
		}
		if msg.Button == tea.MouseButtonWheelDown {
			m.move(3)
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.searching {
			switch msg.String() {
			case "enter":
				m.query, m.searching = m.search.Value(), false
				m.search.Blur()
				m.refresh()
			case "esc":
				m.searching = false
				m.search.Blur()
				m.search.SetValue(m.query)
			default:
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				return m, cmd
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit
		case "/":
			m.searching = true
			return m, m.search.Focus()
		case "f":
			m.follow = !m.follow
			m.refresh()
		case "e":
			m.warnings = !m.warnings
			m.refresh()
		case "r":
			m.raw = !m.raw
			m.refresh()
		case "end", "G":
			m.follow = true
			m.refresh()
		case "home", "g":
			m.follow = false
			m.selected = 0
			m.selectedRaw = ""
			m.refresh()
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "pgup":
			if m.selectedExpanded() {
				m.manualScroll = true
				m.viewport.PageUp()
			} else {
				m.move(-max(m.viewport.Height-1, 1))
			}
		case "pgdown":
			if m.selectedExpanded() {
				m.manualScroll = true
				m.viewport.PageDown()
			} else {
				m.move(max(m.viewport.Height-1, 1))
			}
		case "shift+up":
			m.manualScroll = true
			m.viewport.ScrollUp(1)
		case "shift+down":
			m.manualScroll = true
			m.viewport.ScrollDown(1)
		case "left", "h":
			m.horizontal = max(0, m.horizontal-12)
			m.renderEntries()
		case "right", "l":
			m.horizontal = min(max(0, tableWidth()-max(1, m.width-8)), m.horizontal+12)
			m.renderEntries()
		case "enter", " ":
			if len(m.entries) > 0 {
				m.follow = false
				m.manualScroll = false
				raw := m.entries[m.selected].raw
				m.expanded[raw] = !m.expanded[raw]
				m.renderEntries()
			}
		}
	}
	return m, nil
}

func (m *logsModel) move(delta int) {
	m.follow = false
	m.manualScroll = false
	m.selected = max(0, min(m.selected+delta, len(m.entries)-1))
	m.selectedRaw = ""
	m.renderEntries()
}

func (m logsModel) selectedExpanded() bool {
	return len(m.entries) > 0 && m.expanded[m.entries[m.selected].raw]
}

func (m *logsModel) refresh() {
	m.entries = nil
	retained := map[string]bool{}
	for _, raw := range m.lines {
		var data map[string]any
		_ = json.Unmarshal([]byte(raw), &data)
		level, _ := data["level"].(string)
		if m.warnings && level != "WARN" && level != "ERROR" {
			continue
		}
		if m.query != "" && !strings.Contains(strings.ToLower(raw+" "+eventLabel(data)), strings.ToLower(m.query)) {
			continue
		}
		m.entries = append(m.entries, logEntry{raw, data})
		if m.expanded[raw] {
			retained[raw] = true
		}
	}
	m.expanded = retained
	m.matched = len(m.entries)
	if m.follow {
		m.selected = max(0, len(m.entries)-1)
	} else if m.selectedRaw != "" {
		for i, entry := range m.entries {
			if entry.raw == m.selectedRaw {
				m.selected = i
				break
			}
		}
	}
	m.selected = max(0, min(m.selected, len(m.entries)-1))
	m.renderEntries()
}

func (m *logsModel) renderEntries() {
	var lines []string
	selectedStart, selectedEnd := 0, 0
	for i, entry := range m.entries {
		selected := i == m.selected
		if selected {
			selectedStart = len(lines)
			m.selectedRaw = entry.raw
		}
		lines = append(lines, renderLogRow(entry, i+1, selected, m.width, m.horizontal, m.raw))
		if m.expanded[entry.raw] {
			for _, detail := range logDetails(entry, m.raw) {
				wrapped := ansi.Wrap(cleanLogText(detail), max(1, m.width-3), "")
				for _, line := range strings.Split(wrapped, "\n") {
					lines = append(lines, ansi.Truncate("   "+line, m.width, ""))
				}
			}
		}
		if selected {
			selectedEnd = len(lines)
		}
	}
	if m.err != nil {
		lines = append(lines, ansi.Truncate("无法读取日志："+cleanLogText(m.err.Error()), m.width, "…"))
	}
	if len(lines) == 0 {
		lines = append(lines, ansi.Truncate("本次启动暂无匹配的日志", m.width, "…"))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if m.manualScroll && !m.follow {
		return
	}
	if m.follow {
		m.viewport.GotoBottom()
	} else if selectedStart < m.viewport.YOffset {
		m.viewport.SetYOffset(selectedStart)
	} else if selectedEnd > m.viewport.YOffset+m.viewport.Height {
		m.viewport.SetYOffset(selectedStart)
	}
}

func (m logsModel) View() string {
	mode, follow := "全部级别", "自动跟随"
	if m.warnings {
		mode = "告警与错误"
	}
	if !m.follow {
		follow = "已暂停"
	}
	filter := "搜索  " + m.query
	if m.searching {
		filter = m.search.View()
	}
	line := func(text string) string { return ansi.Truncate(text, m.width, "") }
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(line(m.title + " · 仅本次启动"))
	return strings.Join([]string{title, renderTableHeader(m.width, m.horizontal, m.raw), m.viewport.View(), line(filter),
		line(fmt.Sprintf("%s · %s · %d 条 · 横移 %d", mode, follow, m.matched, m.horizontal)),
		line("↑↓/滚轮选择 ←→横移 Enter展开 /搜索 e告警 f跟随 r原始 q退出")}, "\n")
}
