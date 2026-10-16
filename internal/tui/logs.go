package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
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

type logsModel struct {
	path      string
	title     string
	width     int
	height    int
	viewport  viewport.Model
	search    textinput.Model
	searching bool
	query     string
	follow    bool
	warnings  bool
	raw       bool
	lines     []string
	err       error
	matched   int
}

func ViewLogs(ctx context.Context, path, title string) error {
	if path == "" {
		return errors.New("尚未设置日志文件")
	}
	_, err := tea.NewProgram(newLogsModel(path, title), tea.WithOutput(os.Stderr), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func newLogsModel(path, title string) logsModel {
	search := textinput.New()
	search.Prompt = "搜索  "
	search.CharLimit = 160
	return logsModel{path: path, title: title, width: 80, height: 24, viewport: viewport.New(80, 19), search: search, follow: true}
}

func (m logsModel) Init() tea.Cmd { return readLogs(m.path) }

func readLogs(path string) tea.Cmd {
	return func() tea.Msg {
		lines, err := tailLog(path)
		return logMessage{lines: lines, err: err}
	}
}

func tailLog(path string) ([]string, error) {
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
	text := strings.TrimRight(strings.ToValidUTF8(string(data), "?"), "\r\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	return lines, nil
}

func (m logsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 1), max(msg.Height, 1)
		m.viewport.Width, m.viewport.Height = m.width, max(m.height-5, 1)
		m.search.Width = max(m.width-8, 1)
		m.refresh()
	case logMessage:
		m.lines, m.err = msg.lines, msg.err
		m.refresh()
		return m, tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return logTick{} })
	case logTick:
		return m, readLogs(m.path)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.searching {
			switch msg.String() {
			case "enter":
				m.query = m.search.Value()
				m.searching = false
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
			m.viewport.GotoBottom()
		case "up", "pgup", "home", "k", "g":
			m.follow = false
			m.viewport, _ = m.viewport.Update(msg)
		default:
			m.viewport, _ = m.viewport.Update(msg)
		}
	}
	return m, nil
}

func (m *logsModel) refresh() {
	var lines []string
	m.matched = 0
	for _, raw := range m.lines {
		var data map[string]any
		_ = json.Unmarshal([]byte(raw), &data)
		level, _ := data["level"].(string)
		if m.warnings && level != "WARN" && level != "ERROR" {
			continue
		}
		if m.query != "" && !strings.Contains(strings.ToLower(raw), strings.ToLower(m.query)) {
			continue
		}
		line := raw
		if !m.raw && data != nil {
			line = compactLog(data)
		}
		line = strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, ansi.Strip(line))
		lines = append(lines, ansi.Wrap(line, m.width, ""))
		m.matched++
	}
	if m.err != nil {
		lines = append(lines, "无法读取日志: "+m.err.Error())
	}
	if len(lines) == 0 {
		lines = append(lines, "暂无匹配的日志")
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

func compactLog(data map[string]any) string {
	var parts []string
	if timestamp, ok := data["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
			parts = append(parts, parsed.Local().Format("15:04:05"))
		}
	}
	for _, name := range []string{"level", "msg"} {
		if value, ok := data[name].(string); ok {
			parts = append(parts, value)
		}
	}
	var keys []string
	for key := range data {
		if key != "time" && key != "level" && key != "msg" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, data[key]))
	}
	return strings.Join(parts, "  ")
}

func (m logsModel) View() string {
	mode, follow, format := "全部级别", "自动跟随", "紧凑"
	if m.warnings {
		mode = "告警与错误"
	}
	if !m.follow {
		follow = "已暂停"
	}
	if m.raw {
		format = "原始 JSON"
	}
	footer := fmt.Sprintf("%s · %s · %s · %d 条", mode, follow, format, m.matched)
	filter := "搜索  " + m.query
	if m.searching {
		filter = m.search.View()
	}
	line := func(text string) string { return ansi.Truncate(text, m.width, "") }
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(line(m.title))
	return strings.Join([]string{title, line(m.path), m.viewport.View(), line(filter), line(footer)}, "\n")
}
