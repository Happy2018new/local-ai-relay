package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

var logRenderer = func() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(os.Stderr)
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		r.SetColorProfile(termenv.Ascii)
	} else {
		r.SetColorProfile(termenv.ANSI256)
	}
	return r
}()

type logColumn struct {
	title string
	width int
	color string
}

var logColumns = []logColumn{
	{"时间", 8, "244"}, {"级别", 4, "39"}, {"事件", 14, "81"},
	{"模型", 22, "75"}, {"推理", 7, "141"}, {"请求流式", 8, "45"}, {"上游SSE", 7, "43"},
	{"首字", 9, "214"}, {"总耗时", 9, "180"},
	{"输入未命中", 12, "75"}, {"输出", 10, "78"}, {"缓存读", 10, "117"}, {"缓存写", 10, "176"}, {"总Token", 11, "221"},
	{"原因 / 状态", 90, "250"},
}

func tableWidth() int {
	n := 0
	for _, c := range logColumns {
		n += c.width + 2
	}
	return n
}
func cleanLogText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r >= 0x80 && r <= 0x9f {
			return ' '
		}
		return r
	}, ansi.Strip(value))
}
func cell(text string, width int) string {
	text = ansi.Truncate(cleanLogText(text), width, "…")
	return text + strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
}
func paint(text, color string, selected bool) string {
	style := logRenderer.NewStyle().Foreground(lipgloss.Color(color))
	if selected {
		style = style.Background(lipgloss.Color("236")).Bold(true)
	}
	return style.Render(text)
}
func renderTableHeader(width, offset int, raw bool) string {
	var fields []string
	if raw {
		fields = append(fields, paint("原始 JSON（选中后 Enter 展开）", "81", false))
	} else {
		for _, c := range logColumns {
			fields = append(fields, paint(cell(c.title, c.width)+"  ", c.color, false))
		}
	}
	return ansi.Truncate(paint("  行号  ", "244", false)+ansi.Cut(strings.Join(fields, ""), offset, offset+max(0, width-8)), width, "")
}

func renderLogRow(entry logEntry, row int, selected bool, width, offset int, raw bool) string {
	marker := " "
	if selected {
		marker = "›"
	}
	prefix := paint(fmt.Sprintf("%s%5d  ", marker, row), "244", selected)
	var body string
	if raw {
		body = paint(cleanLogText(entry.raw), "117", selected)
	} else {
		values := logValues(entry.data)
		var fields []string
		for i, c := range logColumns {
			color := c.color
			if i == 2 {
				color = eventColor(entry.data)
			}
			if i == 1 || i == len(logColumns)-1 {
				switch entry.data["level"] {
				case "WARN":
					color = "220"
				case "ERROR":
					color = "203"
				}
			}
			fields = append(fields, paint(cell(values[i], c.width)+"  ", color, selected))
		}
		body = strings.Join(fields, "")
	}
	return ansi.Truncate(prefix+ansi.Cut(body, offset, offset+max(0, width-8)), width, "")
}

func eventColor(data map[string]any) string {
	switch data["msg"] {
	case "relay_started":
		return "43"
	case "relay_stopped":
		return "244"
	case "request_received":
		return "75"
	case "upstream_response":
		if number(data["status"]) >= 400 {
			return statusColor(data["status"])
		}
		return "45"
	case "first_output":
		return "221"
	case "token_usage":
		return "141"
	case "request_finished":
		if data["outcome"] == "canceled" {
			return "215"
		}
		if number(data["status"]) >= 400 || data["outcome"] != "completed" {
			return "203"
		}
		return "78"
	case "upstream_attempt":
		if data["will_retry"] == true {
			return "220"
		}
		return "203"
	case "stream_aborted", "response_failed":
		return "203"
	}
	return severityColor(data)
}

func severityColor(data map[string]any) string {
	switch data["level"] {
	case "ERROR":
		return "203"
	case "WARN":
		return "220"
	default:
		return "117"
	}
}

func statusColor(value any) string {
	switch status := number(value); {
	case status == 429:
		return "220"
	case status >= 400:
		return "203"
	case status >= 300:
		return "215"
	case status >= 200:
		return "78"
	default:
		return "244"
	}
}

func eventLabel(data map[string]any) string {
	label := map[string]string{"relay_started": "代理已启动", "relay_stopped": "代理已停止", "request_received": "收到请求", "upstream_response": "上游已响应",
		"first_output":     "首次输出",
		"upstream_attempt": "上游请求失败", "stream_aborted": "响应传输中断", "token_usage": "Token统计", "response_failed": "上游生成失败"}[textValue(data["msg"])]
	if data["msg"] == "request_finished" {
		if data["outcome"] == "canceled" {
			return "请求已取消"
		}
		if number(data["status"]) >= 400 || data["outcome"] != "completed" {
			return "请求失败"
		}
		return "请求完成"
	}
	if label == "" {
		return "日志消息"
	}
	return label
}
func textValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
func number(value any) float64 {
	if n, ok := value.(float64); ok {
		return n
	}
	return 0
}
func numericText(value any) string {
	if value == nil {
		return "未报告"
	}
	text := strconv.FormatInt(int64(number(value)), 10)
	for i := len(text) - 3; i > 0; i -= 3 {
		text = text[:i] + "," + text[i:]
	}
	return text
}
func durationText(value any) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.2fs", number(value)/1000)
}
func boolText(value any) string {
	if b, ok := value.(bool); ok {
		if b {
			return "是"
		}
		return "否"
	}
	return "未确认"
}
func object(value any) map[string]any { m, _ := value.(map[string]any); return m }
func usageValues(data map[string]any) (input, output, read, write, total string) {
	u := object(data["usage"])
	d := object(u["input_tokens_details"])
	input = "未报告"
	if u["input_tokens"] != nil && d["cached_tokens"] != nil {
		n := number(u["input_tokens"]) - number(d["cached_tokens"])
		if n >= 0 {
			input = numericText(n)
		}
	}
	output, read, write, total = numericText(u["output_tokens"]), numericText(d["cached_tokens"]), numericText(d["cache_write_tokens"]), numericText(u["total_tokens"])
	if u["total_tokens"] == nil && u["input_tokens"] != nil && u["output_tokens"] != nil {
		total = numericText(number(u["input_tokens"]) + number(u["output_tokens"]))
	}
	return
}
func logValues(data map[string]any) []string {
	timestamp := "—"
	if t, err := time.Parse(time.RFC3339Nano, textValue(data["time"])); err == nil {
		timestamp = t.Local().Format("15:04:05")
	}
	level := map[string]string{"INFO": "信息", "WARN": "警告", "ERROR": "错误", "DEBUG": "调试"}[textValue(data["level"])]
	if level == "" {
		level = "信息"
	}
	model, effort := textValue(data["model"]), textValue(data["reasoning_effort"])
	if model == "" {
		model = "—"
	}
	if effort == "" {
		effort = "未指定"
	}
	input, output, read, write, total := usageValues(data)
	reason := textValue(data["reason"])
	if reason == "" {
		switch data["msg"] {
		case "relay_started":
			reason = "监听 " + textValue(data["listen"]) + "；强制流模式：" + boolText(data["require_stream"])
		case "request_received":
			reason = "已读取请求，等待排队及上游响应"
		case "upstream_response":
			reason = "HTTP " + numericText(data["status"]) + "；收到上游响应头"
		case "request_finished":
			reason = "响应传输完成"
		case "token_usage":
			reason = "已取得上游报告的 Token 用量"
		case "first_output":
			reason = "已收到首个输出增量，正在持续转发"
		default:
			reason = "服务消息：" + textValue(data["msg"])
		}
	}
	return []string{timestamp, level, eventLabel(data), model, effort, boolText(data["request_stream"]), boolText(data["upstream_sse"]), durationText(data["first_output_ms"]), durationText(data["duration_ms"]), input, output, read, write, total, reason}
}
