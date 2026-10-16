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
	raw    string
	data   map[string]any
	id     string
	events []logEntry
}

func (e logEntry) identity() string {
	if e.id != "" {
		return e.id
	}
	return e.raw
}

type logRowKey struct{ entryID, detail string }

type logViewRow struct {
	key, parent  logRowKey
	entry, depth int
	detail       logDetail
	expandable   bool
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
	selectedEntry                 string
	selectedDetail                string
	rows                          []logViewRow
	detailExpanded                map[logRowKey]bool
	detailOffsets                 map[logRowKey]int
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
		viewport: viewport.New(80, 18), search: search, follow: true, expanded: map[string]bool{},
		detailExpanded: map[logRowKey]bool{}, detailOffsets: map[logRowKey]int{}}
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
			m.selectedEntry, m.selectedDetail = "", ""
			m.refresh()
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "pgup":
			m.move(-max(m.viewport.Height-1, 1))
		case "pgdown":
			m.move(max(m.viewport.Height-1, 1))
		case "shift+up":
			m.manualScroll = true
			m.viewport.ScrollUp(1)
		case "shift+down":
			m.manualScroll = true
			m.viewport.ScrollDown(1)
		case "left", "h":
			m.scrollHorizontal(-12)
		case "right", "l":
			m.scrollHorizontal(12)
		case "enter", " ":
			m.toggleSelected()
		case "backspace":
			m.collapseOrParent()
		}
	}
	return m, nil
}

func (m *logsModel) move(delta int) {
	m.follow = false
	m.manualScroll = false
	m.selectRow(max(0, min(m.selected+delta, len(m.rows)-1)))
	m.renderEntries()
}

func (m *logsModel) selectRow(index int) {
	m.selected = index
	if index < len(m.rows) {
		m.selectedEntry, m.selectedDetail = m.rows[index].key.entryID, m.rows[index].key.detail
	}
}

func (m logsModel) rowExpanded(row logViewRow) bool {
	if row.depth == 0 {
		return m.expanded[row.key.entryID]
	}
	return m.detailExpanded[row.key]
}

func (m *logsModel) toggleSelected() {
	if len(m.rows) == 0 {
		return
	}
	row := m.rows[m.selected]
	if !row.expandable {
		return
	}
	m.follow, m.manualScroll = false, false
	if row.depth == 0 {
		opening := !m.expanded[row.key.entryID]
		m.expanded[row.key.entryID] = opening
		// Show the event timeline on the first expansion of a request.
		history := logRowKey{entryID: row.key.entryID, detail: "/history"}
		if _, seen := m.detailExpanded[history]; opening && !seen && len(m.entries[row.entry].events) > 0 {
			m.detailExpanded[history] = true
		}
	} else {
		m.detailExpanded[row.key] = !m.detailExpanded[row.key]
	}
	m.renderEntries()
}

func (m *logsModel) collapseOrParent() {
	if len(m.rows) == 0 {
		return
	}
	row := m.rows[m.selected]
	if m.rowExpanded(row) {
		m.toggleSelected()
		return
	}
	if row.depth > 0 {
		m.follow, m.manualScroll = false, false
		m.selectedEntry, m.selectedDetail = row.parent.entryID, row.parent.detail
		m.renderEntries()
	}
}

func (m *logsModel) scrollHorizontal(delta int) {
	if len(m.rows) > 0 && m.rows[m.selected].depth > 0 {
		row := m.rows[m.selected]
		_, _, available := detailLayout(m.width, row.depth, row.detail.label != "")
		limit := max(0, ansi.StringWidth(cleanLogText(row.detail.value))-available)
		m.detailOffsets[row.key] = max(0, min(limit, m.detailOffsets[row.key]+delta))
	} else {
		limit := max(0, tableWidth()-max(1, m.width-8))
		if m.raw && len(m.rows) > 0 {
			limit = max(0, ansi.StringWidth(cleanLogText(m.entries[m.rows[m.selected].entry].raw))-max(1, m.width-8))
		}
		m.horizontal = max(0, min(limit, m.horizontal+delta))
	}
	m.renderEntries()
}

func (m *logsModel) refresh() {
	m.entries = nil
	retained := map[string]bool{}
	active := map[string]bool{}
	for _, entry := range groupLogEntries(m.lines) {
		if !entry.matches(m.query, m.warnings) {
			continue
		}
		m.entries = append(m.entries, entry)
		id := entry.identity()
		active[id] = true
		if m.expanded[id] {
			retained[id] = true
		}
	}
	m.expanded = retained
	for key := range m.detailExpanded {
		if !active[key.entryID] {
			delete(m.detailExpanded, key)
		}
	}
	for key := range m.detailOffsets {
		if !active[key.entryID] {
			delete(m.detailOffsets, key)
		}
	}
	m.matched = len(m.entries)
	m.renderEntries()
}

func (m *logsModel) buildRows() {
	m.rows = nil
	var appendDetails func([]logDetail, int, int, logRowKey)
	appendDetails = func(details []logDetail, entry, depth int, parent logRowKey) {
		for _, detail := range details {
			key := logRowKey{entryID: parent.entryID, detail: parent.detail + "/" + detail.id}
			expandable := detailExpandable(detail, m.width, depth) || m.detailExpanded[key]
			m.rows = append(m.rows, logViewRow{key: key, parent: parent, entry: entry, depth: depth, detail: detail, expandable: expandable})
			if m.detailExpanded[key] {
				if len(detail.children) > 0 {
					appendDetails(detail.children, entry, depth+1, key)
				} else {
					for _, line := range detailTextLines(detail, m.width, depth+1) {
						lineKey := logRowKey{entryID: parent.entryID, detail: key.detail + "/text/" + line.id}
						m.rows = append(m.rows, logViewRow{key: lineKey, parent: key, entry: entry, depth: depth + 1, detail: line})
					}
				}
			}
		}
	}
	for i, entry := range m.entries {
		key := logRowKey{entryID: entry.identity()}
		m.rows = append(m.rows, logViewRow{key: key, entry: i, expandable: true})
		if m.expanded[entry.identity()] {
			appendDetails(entryDetails(entry, m.raw), i, 1, key)
		}
	}
}

func (m *logsModel) renderEntries() {
	m.buildRows()
	if m.follow {
		for i := len(m.rows) - 1; i >= 0; i-- {
			if m.rows[i].depth == 0 {
				m.selected = i
				break
			}
		}
	} else if m.selectedEntry != "" {
		for i, row := range m.rows {
			if row.key.entryID == m.selectedEntry {
				// Fall back to the parent record if a filter or mode hides the detail.
				if row.depth == 0 || row.key.detail == m.selectedDetail {
					m.selected = i
				}
				if row.key.detail == m.selectedDetail {
					break
				}
			}
		}
	}
	m.selectRow(max(0, min(m.selected, len(m.rows)-1)))
	var lines []string
	for i, row := range m.rows {
		selected := i == m.selected
		if row.depth == 0 {
			lines = append(lines, renderLogRow(m.entries[row.entry], row.entry+1, selected, m.width, m.horizontal, m.raw))
		} else {
			_, _, available := detailLayout(m.width, row.depth, row.detail.label != "")
			offset := min(m.detailOffsets[row.key], max(0, ansi.StringWidth(cleanLogText(row.detail.value))-available))
			lines = append(lines, renderDetailRow(row.detail, row.depth, m.width, offset, selected, row.expandable, m.rowExpanded(row)))
		}
	}
	if m.err != nil {
		lines = append(lines, paint(ansi.Truncate("无法读取日志："+cleanLogText(m.err.Error()), m.width, "…"), "203", false))
	}
	if len(lines) == 0 {
		lines = append(lines, ansi.Truncate("本次启动暂无匹配的日志", m.width, "…"))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if m.manualScroll && !m.follow {
		return
	}
	if m.follow {
		m.viewport.SetYOffset(max(0, m.selected-m.viewport.Height+1))
	} else if m.selected < m.viewport.YOffset {
		m.viewport.SetYOffset(m.selected)
	} else if m.selected >= m.viewport.YOffset+m.viewport.Height {
		m.viewport.SetYOffset(m.selected - m.viewport.Height + 1)
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
	offset, scope := m.horizontal, "表格"
	if len(m.rows) > 0 && m.rows[m.selected].depth > 0 {
		offset, scope = m.detailOffsets[m.rows[m.selected].key], "当前行"
	}
	return strings.Join([]string{title, renderTableHeader(m.width, m.horizontal, m.raw), m.viewport.View(), line(filter),
		line(fmt.Sprintf("%s · %s · %d 条 · %s横移 %d", mode, follow, m.matched, scope, offset)),
		line("↑↓选择 ←→横移 Enter展开 退格返回 /搜索 e告警 f跟随 r原始 q退出")}, "\n")
}
