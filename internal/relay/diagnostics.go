package relay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"syscall"
	"time"
)

var errStreamIdle = errors.New("relay stream idle timeout")
var errResponseHeaders = errors.New("relay response header timeout")

func unknownBool(value *bool) any {
	if value == nil {
		return "unknown"
	}
	return *value
}

func (p *Relay) logRequest(state *requestState, level slog.Level, event string, fields ...any) {
	base := []any{"request_id", state.id, "route", state.route, "model", state.metadata.Model,
		"reasoning_effort", state.metadata.Reasoning.Effort, "require_stream", p.cfg.RequireStream,
		"request_stream", unknownBool(state.metadata.Stream), "upstream_sse", unknownBool(state.upstreamSSE),
		"duration_ms", time.Since(state.started).Milliseconds()}
	if state.firstByteMS != nil {
		base = append(base, "first_byte_ms", *state.firstByteMS)
	}
	if state.firstOutputMS != nil {
		base = append(base, "first_output_ms", *state.firstOutputMS)
	}
	if state.usage != nil {
		base = append(base, "usage", state.usage)
	}
	if state.reason != "" {
		base = append(base, "reason_code", state.reasonCode, "reason", state.reason)
	}
	p.logger.Log(context.Background(), level, event, append(base, fields...)...)
}

func upstreamReason(status int, metadata errorMetadata) string {
	var reason string
	switch metadata.Hint {
	case "quota":
		reason = "上游额度或账单限制，自动重试通常无法恢复"
	case "concurrency":
		reason = "上游限制同时处理的请求数，请降低并发"
	case "token_rate":
		reason = "上游限制单位时间内的 Token 用量，请等待额度恢复或减少上下文"
	case "request_rate":
		reason = "上游限制请求频率，请等待冷却或增大请求间隔"
	case "capacity":
		reason = "上游服务容量不足或暂时没有可用通道"
	default:
		switch status {
		case 400:
			reason = "上游拒绝请求参数，请检查模型、推理强度和请求格式"
		case 401:
			reason = "上游认证失败，请检查 API Key"
		case 403:
			reason = "上游拒绝访问，请检查账号和模型权限"
		case 404:
			reason = "上游接口或模型不存在，请检查 API 根地址和模型名称"
		case 408, 504:
			reason = "上游处理超时"
		case 429:
			reason = "上游返回限流响应，具体限制类别未报告"
		case 500:
			reason = "上游内部处理失败，可能需要稍后重试或联系服务商"
		case 502:
			reason = "上游网关未能取得有效响应，可能是服务商内部连接故障"
		case 503, 529:
			reason = "上游服务暂时不可用或容量不足"
		case 301, 302, 303, 307, 308:
			reason = "上游要求重定向；代理保留该响应，请检查 API 根地址"
		default:
			reason = "上游返回错误响应，未提供可确认的具体原因"
		}
	}
	return fmt.Sprintf("HTTP %d：%s（错误码 %s；类型 %s）", status, reason, displayIdentifier(metadata.Code), displayIdentifier(metadata.Type))
}

func displayIdentifier(value string) string {
	if value == "" {
		return "未报告"
	}
	return value
}

func generationReason(metadata errorMetadata) string {
	switch metadata.Code {
	case "max_output_tokens":
		return "生成达到请求设置的最大输出 Token 数量，响应未完整生成"
	case "content_filter":
		return "上游内容过滤器终止了生成"
	case "server_error":
		return "上游在生成过程中发生内部错误"
	default:
		return "上游在 SSE 流中报告生成失败或未完成：" + upstreamReason(200, metadata)
	}
}

func transportReason(err error, ctx context.Context) (string, string) {
	switch {
	case errors.Is(context.Cause(ctx), errResponseHeaders):
		return "response_header_timeout", "等待上游响应头超时，请检查上游负载或调整响应头超时"
	case errors.Is(context.Cause(ctx), errStreamIdle):
		return "stream_idle_timeout", "响应流在设定的空闲时间内没有新数据，代理已取消上游连接"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof", "上游连接在响应传输完成前关闭，已输出的内容不会自动重放"
	case errors.Is(err, io.EOF):
		return "connection_closed", "上游在返回响应前关闭了连接"
	case errors.Is(err, context.Canceled):
		return "request_canceled", "请求已被客户端取消，或代理正在停止"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", "上游拒绝连接，请检查上游地址、端口和服务状态"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset", "上游或网络设备重置了连接"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error", "无法解析上游域名，请检查上游地址和 DNS/网络设置"
	}
	var cert x509.UnknownAuthorityError
	if errors.As(err, &cert) {
		return "tls_certificate_error", "上游 TLS 证书无法通过验证，请检查证书和系统信任设置"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "network_timeout", "与上游通信超时，请检查网络及上游服务状态"
	}
	return "upstream_read_error", "与上游通信时连接异常；无法从现有信号确定更具体的原因"
}

func localReason(code string) string {
	switch code {
	case "stream_required":
		return "请求没有设置 stream:true，已按强制流模式在本地拒绝"
	case "invalid_json":
		return "请求不是有效的 JSON 对象，已在本地拒绝"
	case "invalid_compression":
		return "请求压缩数据无效或解压失败，已在本地拒绝"
	case "unsupported_compression":
		return "请求使用了不支持的压缩格式；支持普通 JSON、gzip 和 zstd"
	case "request_too_large":
		return "请求大小超过配置的上限，已在本地拒绝"
	case "request_read_failed":
		return "读取客户端请求失败，可能是客户端断开或读取超时"
	case "relay_queue_full":
		return "本地队列已满，请降低并发或调整队列上限"
	case "relay_queue_timeout":
		return "等待本地队列或上游冷却超时，请降低并发或调整排队超时"
	default:
		return "请求处理失败，当前没有更具体的诊断信息"
	}
}
