package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
)

const usageEvent = "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":39231,\"output_tokens\":163,\"total_tokens\":39394,\"input_tokens_details\":{\"cached_tokens\":27776,\"cache_write_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":40}}}}\r\n\r\n"

func TestUsageObserverHandlesArbitraryChunksAndBoundedEvents(t *testing.T) {
	for _, chunkSize := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			var usage *TokenUsage
			outputs := 0
			o := streamObserver{onUsage: func(u *TokenUsage) { usage = u }, onOutput: func() { outputs++ }}
			body := ": heartbeat\r\n\r\nevent: ignored\ndata: " + strings.Repeat("x", maxObservedEventBytes+4) + "\n\nevent: response.output_text.delta\ndata: {\"delta\":\"private text\"}\n\n" + usageEvent
			for start := 0; start < len(body); start += chunkSize {
				o.feed([]byte(body[start:min(start+chunkSize, len(body))]))
			}
			o.finish()
			if usage == nil || *usage.InputTokens != 39231 || *usage.OutputTokens != 163 || *usage.InputDetails.CachedTokens != 27776 || usage.InputDetails.CacheWriteTokens == nil || *usage.InputDetails.CacheWriteTokens != 0 || outputs != 1 {
				t.Fatal("fragmented usage, explicit zero, or output timing was lost")
			}
			if len(o.line) > maxObservedEventBytes || len(o.data) > maxObservedEventBytes {
				t.Fatal("observer exceeded memory bounds")
			}
		})
	}
}

func TestUsageUnknownDoesNotBecomeZeroAndFailureIsObserved(t *testing.T) {
	var usage *TokenUsage
	var failure errorMetadata
	o := streamObserver{onUsage: func(u *TokenUsage) { usage = u }, onFailure: func(m errorMetadata) { failure = m }}
	o.feed([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"private text\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n"))
	if usage == nil || usage.InputDetails.CachedTokens != nil || usage.InputDetails.CacheWriteTokens != nil || *usage.OutputTokens != 0 || failure.Code != "server_error" {
		t.Fatal("missing counts or SSE errors were misclassified")
	}
	o.feed([]byte("data: {\"usage\":{\"input_tokens\":-1}}\n\n"))
	if *usage.InputTokens != 12 {
		t.Fatal("invalid negative usage replaced reported counts")
	}
}

func TestRequestLogsContainModelStreamTimingUsageAndNoContent(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	wire := ": heartbeat\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"private completion\"}\n\n" + usageEvent
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, ": heartbeat\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(15 * time.Millisecond)
		for _, b := range []byte(strings.TrimPrefix(wire, ": heartbeat\n\n")) {
			w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
	}, nil, config.Secrets{}, logger)
	request := strings.Replace(streamRequest, `"stream":true`, `"stream":true,"reasoning":{"effort":"high"}`, 1)
	response := post(t, server.Client(), server.URL, []byte(request), nil)
	if bodyOf(t, response) != wire {
		t.Fatal("observing usage changed forwarded bytes")
	}
	eventually(t, func() bool {
		return p.counts.succeeded.Load() == 1 && strings.Contains(logs.String(), `"msg":"request_finished"`)
	})
	var finished map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "request_finished" {
			finished = record
		}
	}
	if finished["model"] != "test-model" || finished["reasoning_effort"] != "high" || finished["request_stream"] != true || finished["upstream_sse"] != true {
		t.Fatal("request metadata missing from log")
	}
	if finished["first_output_ms"] == nil || finished["first_byte_ms"] == nil || finished["first_output_ms"].(float64) < finished["first_byte_ms"].(float64) || finished["duration_ms"].(float64) < finished["first_output_ms"].(float64) {
		t.Fatal("first byte/output/total timing is inconsistent")
	}
	usage := finished["usage"].(map[string]any)
	if usage["total_tokens"] != float64(39394) {
		t.Fatal("usage not recorded")
	}
	for _, private := range []string{"private prompt", "private completion"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("request or response content logged")
		}
	}
}

func TestIdleStreamLogExplainsTimeout(t *testing.T) {
	var logs lockedBuffer
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, func(c *config.Config) { c.StreamIdleTimeout = config.Duration(25 * time.Millisecond) }, config.Secrets{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()
	eventually(t, func() bool {
		return p.counts.failed.Load() == 1 && strings.Contains(logs.String(), `"msg":"request_finished"`)
	})
	if !strings.Contains(logs.String(), `"reason_code":"stream_idle_timeout"`) || !strings.Contains(logs.String(), "空闲时间") {
		t.Fatal("stream abort reason is missing")
	}
}

func TestModeFailuresAndNonStreamingUsageAreLoggedTruthfully(t *testing.T) {
	for _, scenario := range []struct {
		name                  string
		strict, requestStream bool
		contentType, body     string
		status                int
		reason                string
	}{
		{"local nonstream rejection", true, false, "text/event-stream", completeEvent, 400, "stream_required"},
		{"non-SSE success rejected", true, true, "application/json", `{"output":[]}`, 502, "upstream_not_streaming"},
		{"ordinary JSON usage", false, false, "application/json", `{"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}`, 200, ""},
		{"SSE failure", true, true, "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"private body\"}}}\n\n", 200, "server_error"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var logs lockedBuffer
			p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", scenario.contentType)
				fmt.Fprint(w, scenario.body)
			}, func(c *config.Config) { c.RequireStream = scenario.strict }, config.Secrets{}, slog.New(slog.NewJSONHandler(&logs, nil)))
			response := post(t, server.Client(), server.URL, []byte(fmt.Sprintf(`{"model":"test","stream":%t}`, scenario.requestStream)), nil)
			bodyOf(t, response)
			if response.StatusCode != scenario.status {
				t.Fatal("unexpected response status")
			}
			eventually(t, func() bool { return strings.Contains(logs.String(), `"msg":"request_finished"`) })
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] == "WARN" || record["level"] == "ERROR" {
					if record["reason"] == nil || record["reason"] == "" {
						t.Fatal("warning/error missing explanation")
					}
				}
				if record["msg"] == "request_finished" {
					if record["request_stream"] != scenario.requestStream {
						t.Fatal("requested stream mode mislabeled")
					}
					if scenario.reason != "" && record["reason_code"] != scenario.reason {
						t.Fatal("failure reason code missing")
					}
					if !scenario.strict && record["usage"].(map[string]any)["total_tokens"] != float64(11) {
						t.Fatal("JSON usage missing")
					}
				}
			}
			if scenario.reason == "server_error" && p.counts.failed.Load() != 1 {
				t.Fatal("HTTP 200 SSE failure counted as success")
			}
			if strings.Contains(logs.String(), "private body") {
				t.Fatal("upstream error text leaked into logs")
			}
		})
	}
}
