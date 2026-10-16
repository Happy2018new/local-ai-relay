package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type logDetail struct {
	id, label, value, color string
	children                []logDetail
}

func entryDetails(entry logEntry, raw bool) []logDetail {
	fields := logDetails(entry, raw)
	if len(entry.events) == 0 {
		return fields
	}
	if raw {
		fields = logDetails(entry.events[len(entry.events)-1], true)
	}
	history := logDetail{id: "history", label: "事件记录", value: fmt.Sprintf("%d 条", len(entry.events)), color: "109"}
	for _, event := range entry.events {
		values := logValues(event.data)
		hash := sha256.Sum256([]byte(event.raw))
		history.children = append(history.children, logDetail{
			id: fmt.Sprintf("%x", hash[:12]), label: values[0], value: eventLabel(event.data) + " · " + values[14],
			color: eventColor(event.data), children: logDetails(event, raw),
		})
	}
	return append([]logDetail{history}, fields...)
}

func logDetails(entry logEntry, raw bool) []logDetail {
	if raw {
		return jsonLogDetails(entry.data)
	}
	d, values := entry.data, logValues(entry.data)
	rows := []logDetail{{id: "event", label: "事件", value: eventLabel(d), color: eventColor(d)}}
	add := func(key, label, value, color string) {
		if d[key] != nil && d[key] != "" {
			rows = append(rows, logDetail{id: key, label: label, value: value, color: color})
		}
	}
	var diagnostics []logDetail
	for _, field := range []struct{ key, label, color string }{
		{"reason", "具体原因", severityColor(d)}, {"reason_code", "诊断代码", "215"},
		{"error_code", "上游错误码", severityColor(d)}, {"error_type", "上游错误类型", severityColor(d)},
		{"error_hint", "限制类别", "215"},
	} {
		if value := textValue(d[field.key]); value != "" {
			diagnostics = append(diagnostics, logDetail{id: field.key, label: field.label, value: value, color: field.color})
		}
	}
	if d["level"] == "WARN" || d["level"] == "ERROR" || len(diagnostics) > 1 {
		label := "错误详情"
		if d["level"] == "WARN" {
			label = "警告详情"
		}
		if len(diagnostics) == 0 {
			diagnostics = append(diagnostics, logDetail{id: "reason", label: "具体原因", value: values[14], color: severityColor(d)})
		}
		rows = append(rows, logDetail{id: "diagnostics", label: label, value: values[14], color: severityColor(d), children: diagnostics})
	} else {
		rows = append(rows, logDetail{id: "reason", label: "状态", value: values[14], color: eventColor(d)})
	}
	add("status", "HTTP 状态", strconv.Itoa(int(number(d["status"]))), statusColor(d["status"]))
	add("request_id", "请求 ID", textValue(d["request_id"]), "117")
	add("model", "模型", values[3], "75")
	add("reasoning_effort", "推理强度", values[4], "141")
	for _, field := range []struct{ key, label string }{
		{"request_stream", "请求流式"}, {"upstream_sse", "上游 SSE"}, {"require_stream", "强制流模式"},
		{"will_retry", "计划重试"}, {"retryable", "允许重试"},
	} {
		add(field.key, field.label, boolText(d[field.key]), booleanColor(d[field.key]))
	}
	for _, field := range []struct{ key, label string }{
		{"first_output_ms", "首字时间"}, {"first_byte_ms", "首个响应字节"}, {"duration_ms", "总耗时"},
		{"queue_ms", "排队耗时"}, {"response_header_ms", "等待响应头"}, {"attempt_ms", "本次尝试耗时"}, {"retry_delay_ms", "重试等待"},
	} {
		add(field.key, field.label, durationText(d[field.key]), "214")
	}
	if u := object(d["usage"]); u != nil {
		rows = append(rows,
			logDetail{id: "input", label: "输入合计", value: numericText(u["input_tokens"]), color: "75"},
			logDetail{id: "uncached", label: "输入未命中", value: values[9], color: "75"},
			logDetail{id: "output", label: "输出", value: values[10], color: "78"},
			logDetail{id: "cache_read", label: "缓存读取", value: values[11], color: "117"},
			logDetail{id: "cache_write", label: "缓存写入", value: values[12], color: "176"},
			logDetail{id: "total", label: "总 Token", value: values[13], color: "221"},
		)
		if out := object(u["output_tokens_details"]); out["reasoning_tokens"] != nil {
			rows = append(rows, logDetail{id: "reasoning_output", label: "推理输出", value: numericText(out["reasoning_tokens"]), color: "141"})
		}
	}
	for _, field := range []struct{ key, label, color string }{
		{"attempt", "本次尝试", "215"}, {"attempts", "总尝试次数", "215"}, {"bytes_out", "转发字节数", "43"},
		{"upstream_request_id", "上游请求 ID", "117"}, {"session_id", "本次启动 ID", "109"},
		{"route", "请求路径", "109"}, {"listen", "监听地址", "43"},
	} {
		value := textValue(d[field.key])
		if _, ok := d[field.key].(float64); ok {
			value = numericText(d[field.key])
		}
		add(field.key, field.label, value, field.color)
	}
	return rows
}

func booleanColor(value any) string {
	switch value {
	case true:
		return "78"
	case false:
		return "215"
	default:
		return "244"
	}
}

// Raw JSON remains navigable as a tree; scalar colors follow JSON value types.
func jsonLogDetails(data map[string]any) []logDetail {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]logDetail, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, jsonLogDetail(key, data[key]))
	}
	return rows
}

func jsonLogDetail(key string, value any) logDetail {
	encoded, _ := json.Marshal(value)
	row := logDetail{id: strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"), label: key, value: string(encoded), color: "244"}
	switch value := value.(type) {
	case map[string]any:
		row.value, row.color, row.children = fmt.Sprintf("{%d 项}", len(value)), "75", jsonLogDetails(value)
	case []any:
		row.value, row.color = fmt.Sprintf("[%d 项]", len(value)), "75"
		for i, item := range value {
			row.children = append(row.children, jsonLogDetail(strconv.Itoa(i), item))
		}
	case string:
		row.value, row.color = value, "78"
	case float64:
		row.color = "117"
	case bool:
		row.color = "141"
	}
	return row
}

func detailLayout(width, depth int, labeled bool) (prefix string, labelWidth, valueWidth int) {
	indent := min(max(depth-1, 0)*2, max(width/4, 0))
	prefix = "        " + strings.Repeat(" ", indent)
	if labeled {
		labelWidth = min(20, max(0, (width-ansi.StringWidth(prefix)-4)/3))
	}
	labelSpace := 0
	if labeled {
		labelSpace = labelWidth + 1
	}
	valueWidth = max(1, width-ansi.StringWidth(prefix)-2-labelSpace)
	return
}

func detailExpandable(detail logDetail, width, depth int) bool {
	_, _, available := detailLayout(width, depth, detail.label != "")
	return len(detail.children) > 0 || strings.ContainsAny(detail.value, "\r\n") || ansi.StringWidth(cleanLogText(detail.value)) > available
}

func detailTextLines(detail logDetail, width, depth int) []logDetail {
	_, _, available := detailLayout(width, depth, false)
	var rows []logDetail
	value := strings.ReplaceAll(detail.value, "\r\n", "\n")
	for _, paragraph := range strings.Split(value, "\n") {
		for _, line := range strings.Split(ansi.Hardwrap(cleanLogText(paragraph), available, true), "\n") {
			rows = append(rows, logDetail{id: strconv.Itoa(len(rows)), value: line, color: detail.color})
		}
	}
	return rows
}

func renderDetailRow(detail logDetail, depth, width, offset int, selected, expandable, expanded bool) string {
	prefix, labelWidth, available := detailLayout(width, depth, detail.label != "")
	if selected {
		prefix = "›" + prefix[1:]
	}
	fold := "·"
	if expandable {
		fold = "▸"
		if expanded {
			fold = "▾"
		}
	}
	label := ""
	if detail.label != "" {
		label = cell(detail.label, labelWidth) + " "
	}
	value := cleanLogText(detail.value)
	end := offset + available
	visible := ansi.Cut(value, offset, end)
	if ansi.StringWidth(value) > end {
		visible = ansi.Truncate(visible, max(0, available-1), "") + "…"
	}
	return ansi.Truncate(paint(prefix+fold+" ", "109", selected)+paint(label+visible, detail.color, selected), width, "")
}
