package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInterleavedRequestEventsBecomeOneRowPerRequest(t *testing.T) {
	lines := []string{
		`{"msg":"relay_started","session_id":"current"}`,
		`{"msg":"request_received","request_id":"a","session_id":"current","model":"gpt-6.1-sol","reasoning_effort":"medium","request_stream":true}`,
		`{"msg":"request_received","request_id":"b","session_id":"current"}`,
		`{"msg":"upstream_attempt","request_id":"a","session_id":"current","level":"WARN","status":429,"reason":"上游限流","error_code":"rate_limit_exceeded","will_retry":true}`,
		`{"msg":"upstream_response","request_id":"a","session_id":"current","level":"INFO","status":200,"upstream_sse":true}`,
		`{"msg":"token_usage","request_id":"a","session_id":"current","first_output_ms":1500,"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":50}}}`,
		`{"msg":"request_finished","request_id":"b","session_id":"current","outcome":"canceled","duration_ms":2000}`,
		`{"msg":"request_finished","request_id":"a","session_id":"current","level":"INFO","status":200,"outcome":"completed","duration_ms":3000}`,
	}
	entries := groupLogEntries(lines)
	if len(entries) != 3 || len(entries[1].events) != 5 || len(entries[2].events) != 2 {
		t.Fatal("interleaved events were not grouped by request")
	}
	a, b := entries[1], entries[2]
	if eventLabel(a.data) != "请求完成" || eventLabel(b.data) != "请求已取消" {
		t.Fatal("request summary did not show its latest status")
	}
	values := logValues(a.data)
	if values[3] != "gpt-6.1-sol" || values[4] != "medium" || values[7] != "1.50s" || values[8] != "3.00s" || values[13] != "120" {
		t.Fatalf("request summary lost earlier metadata or usage: %v", values)
	}
	if a.data["reason"] != nil || a.data["error_code"] != nil || a.data["will_retry"] != nil {
		t.Fatal("successful request still displays stale retry diagnostics")
	}
	if !a.matches("429", true) || !a.matches("上游请求失败", true) || b.matches("", true) {
		t.Fatal("search and warning filters ignored request history")
	}
	m := newLogsModel("relay.log", "日志", "")
	m.lines = lines
	m.refresh()
	if m.viewport.TotalLineCount() != 3 {
		t.Fatal("collapsed view displays multiple lines for the same request")
	}
	separate := groupLogEntries([]string{`{"session_id":"one","request_id":"a"}`, `{"session_id":"two","request_id":"a"}`})
	if len(separate) != 2 {
		t.Fatal("request IDs from separate launches were combined")
	}
}

func TestRequestStatusUpdatesPreserveExpandedDetailsAndTimeline(t *testing.T) {
	enableLogColors(t)
	m := newLogsModel("relay.log", "日志", "")
	m.width, m.viewport.Width = 64, 64
	requestID := "relay-" + strings.Repeat("1234567890", 12)
	m.lines = []string{`{"level":"INFO","msg":"request_received","request_id":"` + requestID + `","model":"gpt-6.1-sol"}`}
	m.refresh()
	m = pressLogKey(m, tea.KeyEnter)
	m = selectLogDetail(t, m, "事件记录")
	m = pressLogKey(m, tea.KeyDown)
	if m.rows[m.selected].detail.color != "75" || !strings.HasPrefix(m.rows[m.selected].detail.value, "收到请求") {
		t.Fatal("first expansion did not show the colored event timeline")
	}
	timelineKey := m.rows[m.selected].key
	updated, _ := m.Update(logMessage{lines: append(m.lines, `{"level":"INFO","msg":"upstream_response","request_id":"`+requestID+`","status":200}`)})
	m = updated.(logsModel)
	if m.rows[m.selected].key != timelineKey || len(m.entries) != 1 || len(m.entries[0].events) != 2 {
		t.Fatal("new event replaced the selected history row or created another request row")
	}
	m = selectLogDetail(t, m, "请求 ID")
	m = pressLogKey(m, tea.KeyRight)
	fieldKey := m.rows[m.selected].key
	updated, _ = m.Update(logMessage{lines: append(m.lines, `{"level":"WARN","msg":"request_finished","request_id":"`+requestID+`","outcome":"canceled","reason":"客户端取消了请求","duration_ms":5000}`)})
	m = updated.(logsModel)
	if m.rows[m.selected].key != fieldKey || m.detailOffsets[fieldKey] != 12 || !m.expanded[fieldKey.entryID] {
		t.Fatal("final status update lost request expansion, selection, or scroll position")
	}
	if eventLabel(m.entries[0].data) != "请求已取消" || len(m.entries[0].events) != 3 {
		t.Fatal("cancel did not update the existing row and timeline")
	}
	for _, row := range m.rows {
		if strings.HasPrefix(row.detail.value, "请求已取消") && row.detail.color == "215" {
			return
		}
	}
	t.Fatal("cancellation event in history did not use the cancellation color")
}
