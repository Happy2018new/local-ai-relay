package tui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func pressLogKey(m logsModel, key tea.KeyType) logsModel {
	updated, _ := m.Update(tea.KeyMsg{Type: key})
	return updated.(logsModel)
}

func selectLogDetail(t *testing.T, m logsModel, label string) logsModel {
	t.Helper()
	for i := 0; i < len(m.rows); i++ {
		if m.rows[m.selected].detail.label == label {
			return m
		}
		m = pressLogKey(m, tea.KeyDown)
	}
	t.Fatalf("detail %q was not reachable with arrow keys", label)
	return m
}

func enableLogColors(t *testing.T) {
	t.Helper()
	previous := logRenderer
	logRenderer = lipgloss.NewRenderer(io.Discard)
	logRenderer.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { logRenderer = previous })
}

func TestEventColumnUsesEventAndOutcomeColors(t *testing.T) {
	enableLogColors(t)
	for _, tc := range []struct {
		event, outcome, label, color string
		status                       float64
		retry                        bool
	}{
		{event: "request_received", label: "收到请求", color: "75"},
		{event: "upstream_response", label: "上游已响应", color: "45", status: 200},
		{event: "upstream_response", label: "上游已响应", color: "220", status: 429},
		{event: "first_output", label: "首次输出", color: "221"},
		{event: "token_usage", label: "Token统计", color: "141"},
		{event: "request_finished", outcome: "completed", label: "请求完成", color: "78", status: 200},
		{event: "request_finished", outcome: "canceled", label: "请求已取消", color: "215", status: 200},
		{event: "request_finished", outcome: "failed", label: "请求失败", color: "203", status: 502},
		{event: "upstream_attempt", label: "上游请求失败", color: "220", status: 429, retry: true},
		{event: "upstream_attempt", label: "上游请求失败", color: "203", status: 429},
		{event: "stream_aborted", label: "响应传输中断", color: "203"},
		{event: "response_failed", label: "上游生成失败", color: "203"},
	} {
		t.Run(tc.event+"/"+tc.outcome+"/"+tc.color, func(t *testing.T) {
			entry := logEntry{data: map[string]any{"msg": tc.event, "outcome": tc.outcome, "status": tc.status, "will_retry": tc.retry, "level": "INFO"}}
			row := renderLogRow(entry, 1, false, 1000, 0, false)
			if !strings.Contains(row, paint(cell(tc.label, 14)+"  ", tc.color, false)) {
				t.Fatalf("event %q did not use color %s: %q", tc.label, tc.color, row)
			}
		})
	}
}

func TestDetailsNavigateScrollIndependentlyAndSurviveRefresh(t *testing.T) {
	m := newLogsModel("relay.log", "运行日志", "")
	m.width, m.viewport.Width = 64, 64
	record := `{"level":"WARN","msg":"stream_aborted","request_id":"relay-` + strings.Repeat("1234567890", 15) + `","reason":"连接异常：` + strings.Repeat("上游在传输完成前关闭连接；", 12) + `","reason_code":"unexpected_eof"}`
	m.lines = []string{record}
	m.refresh()
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "警告详情")
	if !m.rows[m.selected].expandable {
		t.Fatal("warning detail is not expandable")
	}
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "具体原因")
	reasonKey := m.rows[m.selected].key
	m = pressLogKey(m, tea.KeyRight)
	if m.detailOffsets[reasonKey] != 12 || m.horizontal != 0 {
		t.Fatal("detail scrolling changed the table offset")
	}
	m = selectLogDetail(t, m, "请求 ID")
	requestKey := m.rows[m.selected].key
	m = pressLogKey(m, tea.KeyRight)
	m = pressLogKey(m, tea.KeyRight)
	if m.detailOffsets[requestKey] != 24 || m.detailOffsets[reasonKey] != 12 {
		t.Fatal("detail rows did not retain independent offsets")
	}
	updated, _ := m.Update(logMessage{lines: append([]string{`{"msg":"relay_started"}`}, append(m.lines, `{"msg":"request_received"}`)...)})
	m = updated.(logsModel)
	if m.rows[m.selected].key != requestKey || m.follow || m.detailOffsets[requestKey] != 24 {
		t.Fatal("polling lost the detail selection or horizontal position")
	}
	if !m.detailExpanded[logRowKey{entryID: requestKey.entryID, detail: "/diagnostics"}] {
		t.Fatal("polling collapsed nested error details")
	}
	m = pressLogKey(m, tea.KeyBackspace)
	if m.rows[m.selected].depth != 0 {
		t.Fatal("backspace from a field did not select its parent record")
	}
	m = pressLogKey(m, tea.KeyEnter)
	if len(m.rows) != len(m.entries) {
		t.Fatal("collapsing the record left orphaned details")
	}
}

func TestLongDetailsExpandSafelyAndRemainCollapsibleAfterResize(t *testing.T) {
	enableLogColors(t)
	m := newLogsModel("relay.log", "运行日志", "")
	m.lines = []string{`{"level":"ERROR","msg":"response_failed","reason":"上游连接异常。\n请稍后再试。\u001b[2J\u0007","reason_code":"network_timeout","will_retry":true,"retry_delay_ms":2000}`}
	m.refresh()
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "错误详情")
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "具体原因")
	m = pressLogKey(m, tea.KeyEnter)
	if !strings.Contains(m.viewport.View(), paint("上游连接异常。", "203", false)) {
		t.Fatal("expanded error text lost its severity color")
	}
	if strings.Contains(m.viewport.View(), "\x1b[2J") || strings.Contains(m.viewport.View(), "\x07") {
		t.Fatal("expanded details rendered terminal controls")
	}
	for _, width := range []int{1, 24, 40, 80, 160} {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m = updated.(logsModel)
		for _, line := range strings.Split(m.View(), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("expanded detail wider than terminal width %d", width)
			}
		}
	}
	key := m.rows[m.selected].key
	m = pressLogKey(m, tea.KeyEnter)
	if m.detailExpanded[key] {
		t.Fatal("expanded text could not be collapsed after resize")
	}
}

func TestDetailRowsContainFieldsWithoutTutorialParagraphs(t *testing.T) {
	entry := logEntry{data: map[string]any{"msg": "token_usage", "usage": map[string]any{
		"input_tokens": float64(39231), "output_tokens": float64(163), "total_tokens": float64(39394),
		"input_tokens_details": map[string]any{"cached_tokens": float64(27776), "cache_write_tokens": float64(0)},
	}}}
	rows := logDetails(entry, false)
	values := map[string]string{}
	for _, row := range rows {
		values[row.label] = row.value
		if strings.Contains(row.value, "包含排队") || strings.Contains(row.value, "缓存读写属于") || strings.Contains(row.value, "不再重复加") {
			t.Fatal("detail rows still contain tutorial paragraphs")
		}
	}
	for label, expected := range map[string]string{"输入合计": "39,231", "输入未命中": "11,455", "输出": "163", "缓存读取": "27,776", "缓存写入": "0", "总 Token": "39,394"} {
		if values[label] != expected {
			t.Fatalf("field %s = %s, want %s", label, values[label], expected)
		}
	}
}

func TestRawObjectsCanBeExpandedWithoutLosingSelection(t *testing.T) {
	m := newLogsModel("relay.log", "运行日志", "")
	m.raw = true
	m.lines = []string{`{"usage":{"input_tokens":39231,"input_tokens_details":{"cached_tokens":27776}},"msg":"token_usage"}`}
	m.refresh()
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "usage")
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "input_tokens_details")
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "cached_tokens")
	if m.rows[m.selected].detail.value != "27776" {
		t.Fatal("raw nested scalar was not reachable")
	}
	m.refresh()
	if m.rows[m.selected].detail.label != "cached_tokens" {
		t.Fatal("refresh lost the nested JSON selection")
	}
}
