package relay

import (
	"bytes"
	"encoding/json"
)

// TokenUsage contains only upstream-reported counts. Missing values stay unknown.
// Cached reads and writes are included in InputTokens, not added to TotalTokens.
type TokenUsage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	TotalTokens  *int64 `json:"total_tokens,omitempty"`
	InputDetails struct {
		CachedTokens     *int64 `json:"cached_tokens,omitempty"`
		CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	} `json:"output_tokens_details"`
}

func (u *TokenUsage) valid() bool {
	if u == nil {
		return false
	}
	found := false
	for _, n := range []*int64{u.InputTokens, u.OutputTokens, u.TotalTokens, u.InputDetails.CachedTokens, u.InputDetails.CacheWriteTokens, u.OutputDetails.ReasoningTokens} {
		if n != nil {
			if *n < 0 {
				return false
			}
			found = true
		}
	}
	return found
}

const maxObservedEventBytes = 2 << 20

// This observer never controls forwarding. It sees a bounded copy of SSE data,
// extracts numeric usage, and discards text/tool contents without logging them.
type streamObserver struct {
	line, data    []byte
	event         string
	skip, afterCR bool
	lineNonempty  bool
	blockBytes    int
	onOutput      func()
	onUsage       func(*TokenUsage)
	onFailure     func(errorMetadata)
}

func (o *streamObserver) feed(data []byte) {
	for _, b := range data {
		if o.afterCR {
			o.afterCR = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			o.consumeLine()
			o.afterCR = b == '\r'
			continue
		}
		o.blockBytes++
		o.lineNonempty = true
		if o.blockBytes > maxObservedEventBytes {
			o.skip = true
			o.line, o.data = o.line[:0], o.data[:0]
		}
		if !o.skip {
			o.line = append(o.line, b)
		}
	}
}

func (o *streamObserver) consumeLine() {
	if !o.lineNonempty {
		if !o.skip {
			o.observe(o.data, o.event)
		}
		o.data, o.event, o.blockBytes, o.skip = o.data[:0], "", 0, false
		return
	}
	if !o.skip {
		field, value, _ := bytes.Cut(o.line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			o.event = string(value)
		case "data":
			if len(o.data) > 0 {
				o.data = append(o.data, '\n')
			}
			o.data = append(o.data, value...)
		}
	}
	o.line = o.line[:0]
	o.lineNonempty = false
}

func (o *streamObserver) finish() {
	if len(o.line) > 0 {
		o.consumeLine()
	}
	if !o.skip && len(o.data) > 0 {
		o.observe(o.data, o.event)
	}
}

func (o *streamObserver) observe(data []byte, event string) {
	var envelope struct {
		Type     string      `json:"type"`
		Delta    string      `json:"delta"`
		Code     string      `json:"code"`
		Message  string      `json:"message"`
		Usage    *TokenUsage `json:"usage"`
		Response *struct {
			Usage             *TokenUsage     `json:"usage"`
			Error             json.RawMessage `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return
	}
	if envelope.Type != "" {
		event = envelope.Type
	}
	if envelope.Delta != "" && o.onOutput != nil {
		switch event {
		case "response.output_text.delta", "response.function_call_arguments.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.audio.delta":
			o.onOutput()
		}
	}
	usage := envelope.Usage
	if envelope.Response != nil && envelope.Response.Usage != nil {
		usage = envelope.Response.Usage
	}
	if usage.valid() && o.onUsage != nil {
		o.onUsage(usage)
	}
	if o.onFailure != nil {
		switch event {
		case "response.failed", "response.incomplete", "error":
			metadata := describeError(data)
			if event == "error" && envelope.Code != "" {
				metadata = errorMetadata{Code: safeIdentifier(envelope.Code), Hint: errorHint(envelope.Message)}
			}
			if envelope.Response != nil {
				if len(envelope.Response.Error) > 0 {
					wrapped, _ := json.Marshal(map[string]json.RawMessage{"error": envelope.Response.Error})
					metadata = describeError(wrapped)
				}
				if metadata.Code == "" {
					metadata.Code = safeIdentifier(envelope.Response.IncompleteDetails.Reason)
				}
			}
			if metadata.Code == "" {
				metadata.Code = event
			}
			o.onFailure(metadata)
		}
	}
}
