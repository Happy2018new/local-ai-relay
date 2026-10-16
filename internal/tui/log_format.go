package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
		body = cleanLogText(entry.raw)
	} else {
		values := logValues(entry.data)
		var fields []string
		for i, c := range logColumns {
			color := c.color
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

func logDetails(entry logEntry, raw bool) []string {
	if raw {
		var pretty strings.Builder
		encoder := json.NewEncoder(&pretty)
		encoder.SetIndent("", "  ")
		if encoder.Encode(entry.data) == nil {
			return strings.Split(strings.TrimSpace(pretty.String()), "\n")
		}
		return []string{entry.raw}
	}
	d := entry.data
	values := logValues(d)
	lines := []string{"事件：" + eventLabel(d) + "；" + values[len(values)-1],
		"请求：" + textValue(d["request_id"]), "模型：" + values[3] + "；推理强度：" + values[4],
		"请求流式：" + values[5] + "；上游 SSE：" + values[6] + "；强制流模式：" + boolText(d["require_stream"]),
		"首字时间：" + durationText(d["first_output_ms"]) + "；首个响应字节：" + durationText(d["first_byte_ms"]) + "；总耗时：" + durationText(d["duration_ms"]),
		"时间从代理接收请求开始计算，包含排队。首字指首个文字、推理、工具参数或音频输出增量；未观察到则显示 —。"}
	u, out := object(d["usage"]), map[string]any{}
	if u != nil {
		out = object(u["output_tokens_details"])
	}
	lines = append(lines, "输入合计："+numericText(u["input_tokens"])+"；输入未命中："+values[9]+"；输出："+values[10],
		"缓存读取："+values[11]+"；缓存写入："+values[12]+"；总 Token："+values[13]+"；其中推理输出："+numericText(out["reasoning_tokens"]),
		"缓存读写属于输入合计；输入未命中 = 输入合计 − 缓存读取（包含可能的缓存写入）。总 Token 不再重复加缓存。")
	labels := map[string]string{"status": "HTTP 状态", "attempt": "本次尝试", "attempts": "总尝试次数", "queue_ms": "排队耗时（毫秒）", "response_header_ms": "本次等待响应头（毫秒）", "attempt_ms": "本次尝试耗时（毫秒）", "retry_delay_ms": "重试等待（毫秒）", "will_retry": "是否计划重试", "retryable": "是否可重试", "error_code": "上游错误码", "error_type": "上游错误类型", "error_hint": "限制类别线索", "reason_code": "诊断代码", "bytes_out": "转发字节数", "upstream_request_id": "上游请求 ID", "session_id": "本次启动 ID"}
	var keys []string
	for key := range labels {
		if d[key] != nil && d[key] != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fmt.Sprint(d[key])
		if _, ok := d[key].(bool); ok {
			value = boolText(d[key])
		}
		lines = append(lines, labels[key]+"："+value)
	}
	return lines
}
