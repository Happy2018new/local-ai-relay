package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
	"github.com/klauspost/compress/zstd"
)

const streamRequest = `{"model":"test-model","stream":true,"input":"private prompt","future_field":{"x":1}}`
const completeEvent = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\"}}\n\n"

func testRelay(t *testing.T, upstream http.HandlerFunc, change func(*config.Config), secrets config.Secrets, logger *slog.Logger) (*Relay, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.UpstreamURL = up.URL
	cfg.MinInterval = 0
	cfg.InitialBackoff = config.Duration(4 * time.Millisecond)
	cfg.MaxBackoff = config.Duration(8 * time.Millisecond)
	cfg.RetryWindow = config.Duration(2 * time.Second)
	if change != nil {
		change(&cfg)
	}
	p, err := New(cfg, secrets, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	server := httptest.NewUnstartedServer(p)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return p, server
}

func post(t *testing.T, client *http.Client, target string, body []byte, headers http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, target+"/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = headers
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func bodyOf(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func writeStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, completeEvent)
}

func TestRetry429PreservesBodyAndHeaders(t *testing.T) {
	var attempts atomic.Int32
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != streamRequest || r.Header.Get("Authorization") != "Bearer private-key" || r.URL.Query().Get("probe") != "1" {
			t.Error("request bytes, credentials, or query changed")
		}
		if r.Header.Get("X-Hop") != "" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("hop headers or SSE accept incorrect")
		}
		w.Header().Set("X-Request-Id", "upstream-test")
		if attempts.Add(1) < 3 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","type":"requests","message":"private prompt"}}`)
			return
		}
		writeStream(w)
	}, nil, config.Secrets{}, nil)
	headers := http.Header{"Authorization": {"Bearer private-key"}, "Connection": {"X-Hop"}, "X-Hop": {"secret-hop"}}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses?probe=1", strings.NewReader(streamRequest))
	request.Header = headers
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodyOf(t, response); response.StatusCode != 200 || got != completeEvent || attempts.Load() != 3 {
		t.Fatalf("retry result: status=%d attempts=%d body=%s", response.StatusCode, attempts.Load(), got)
	}
	if response.Header.Get("X-Request-Id") != "upstream-test" || response.Header.Get("X-Relay-Request-Id") == "" || p.counts.rateLimited.Load() != 2 {
		t.Fatal("missing diagnostic headers or counters")
	}
}

func TestQuotaAndExhaustedRetriesReturnOriginalError(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "rate_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			var attempts atomic.Int32
			original := `{"error":{"code":"` + code + `","message":"account detail"}}`
			_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("X-Request-Id", "original-id")
				w.WriteHeader(429)
				fmt.Fprint(w, original)
			}, func(c *config.Config) { c.MaxAttempts = 3 }, config.Secrets{}, nil)
			response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
			if got := bodyOf(t, response); response.StatusCode != 429 || got != original || response.Header.Get("X-Request-Id") != "original-id" {
				t.Fatalf("lost upstream error: status=%d body=%s", response.StatusCode, got)
			}
			want := int32(3)
			if code == "insufficient_quota" {
				want = 1
			}
			if attempts.Load() != want {
				t.Fatalf("attempts=%d want=%d", attempts.Load(), want)
			}
		})
	}
}

func TestStreamingFlushCancellationAndPermitLifetime(t *testing.T) {
	var attempts atomic.Int32
	upstreamCanceled := make(chan struct{}, 2)
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		upstreamCanceled <- struct{}{}
	}, nil, config.Secrets{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(streamRequest))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 10)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if p.schedule.status().Active != 1 {
		t.Fatal("permit released before stream completion")
	}
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	second, _ := http.NewRequestWithContext(secondCtx, http.MethodPost, server.URL+"/responses", strings.NewReader(streamRequest))
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := server.Client().Do(second)
		if err == nil {
			result.Body.Close()
		}
	}()
	eventually(t, func() bool { return p.schedule.status().Queued == 1 })
	if attempts.Load() != 1 {
		t.Fatal("second stream bypassed concurrency limit")
	}
	secondCancel()
	<-done
	eventually(t, func() bool { return p.schedule.status().Queued == 0 })
	cancel()
	response.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream cancellation did not reach upstream")
	}
	eventually(t, func() bool { return p.schedule.status().Active == 0 && p.schedule.status().Queued == 0 })
}

func TestBrokenStreamIsNeverReplayed(t *testing.T) {
	var attempts atomic.Int32
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"once\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}, nil, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil || !strings.Contains(string(body), "once") || attempts.Load() != 1 {
		t.Fatalf("broken stream: error=%v attempts=%d body=%q", err, attempts.Load(), body)
	}
}

func TestCompressedRequestsPreserveOriginalBytes(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var raw bytes.Buffer
			switch encoding {
			case "identity":
				raw.WriteString(streamRequest)
			case "gzip":
				encoder := gzip.NewWriter(&raw)
				encoder.Write([]byte(streamRequest))
				encoder.Close()
			case "zstd":
				encoder, _ := zstd.NewWriter(&raw)
				encoder.Write([]byte(streamRequest))
				encoder.Close()
			}
			_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, raw.Bytes()) || r.Header.Get("Content-Encoding") != encoding {
					t.Error("compressed request was modified")
				}
				writeStream(w)
			}, nil, config.Secrets{}, nil)
			response := post(t, server.Client(), server.URL, raw.Bytes(), http.Header{"Content-Encoding": {encoding}})
			if got := bodyOf(t, response); response.StatusCode != 200 || got != completeEvent {
				t.Fatalf("compressed request failed: %d %s", response.StatusCode, got)
			}
		})
	}
}

func TestRequireStreamAndDecodedSizeLimit(t *testing.T) {
	var called atomic.Int32
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) { called.Add(1) }, func(c *config.Config) { c.MaxRequestBytes = 256 }, config.Secrets{}, nil)
	for _, raw := range []string{`{"stream":false}`, `{"stream":true`, `{"input":[]}`, `null`} {
		response := post(t, server.Client(), server.URL, []byte(raw), nil)
		bodyOf(t, response)
		if response.StatusCode != 400 {
			t.Fatalf("invalid stream accepted: %d", response.StatusCode)
		}
	}
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	compressor.Write([]byte(`{"stream":true,"input":"` + strings.Repeat("x", 1024) + `"}`))
	compressor.Close()
	response := post(t, server.Client(), server.URL, encoded.Bytes(), http.Header{"Content-Encoding": {"gzip"}})
	bodyOf(t, response)
	if response.StatusCode != 413 || called.Load() != 0 {
		t.Fatal("oversized decoded body reached upstream")
	}
}

func TestRetryAfterOutsideBudgetAndSharedCooldown(t *testing.T) {
	var attempts atomic.Int32
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}, func(c *config.Config) { c.QueueTimeout = config.Duration(30 * time.Millisecond) }, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	bodyOf(t, response)
	if response.StatusCode != 429 || p.schedule.status().CooldownSeconds < 59 {
		t.Fatal("Retry-After not preserved as shared cooldown")
	}
	response = post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	bodyOf(t, response)
	if response.StatusCode != 503 || attempts.Load() != 1 {
		t.Fatal("next request bypassed cooldown or queue timeout")
	}
}

func TestAuthenticationRedactionStatusAndNoRedirect(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-private" {
			t.Error("local key forwarded to upstream")
		}
		w.Header().Set("Location", "http://must-not-follow.invalid/")
		w.WriteHeader(307)
		fmt.Fprint(w, `{"error":{"code":"upstream_error","message":"private prompt upstream-private local-private"}}`)
	}, nil, config.Secrets{LocalKey: "local-private", UpstreamKey: "upstream-private"}, logger)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	bodyOf(t, response)
	if response.StatusCode != 401 {
		t.Fatal("missing local authentication accepted")
	}
	client := *server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response = post(t, &client, server.URL, []byte(streamRequest), http.Header{"Authorization": {"Bearer local-private"}})
	bodyOf(t, response)
	if response.StatusCode != 307 {
		t.Fatalf("upstream redirect followed: %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/status", nil)
	request.Header.Set("Authorization", "Bearer local-private")
	status, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(bodyOf(t, status)), &data); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private prompt", "upstream-private", "local-private"} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("private data logged: %q", private)
		}
	}
}

type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func TestNonStreamingSuccessIsRejected(t *testing.T) {
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":[]}`)
	}, nil, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	if got := bodyOf(t, response); response.StatusCode != 502 || !strings.Contains(got, "upstream_not_streaming") {
		t.Fatalf("non-SSE success accepted: %d %s", response.StatusCode, got)
	}
}

func TestIdleStreamIsCanceledWithoutRetry(t *testing.T) {
	var attempts atomic.Int32
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, func(c *config.Config) { c.StreamIdleTimeout = config.Duration(30 * time.Millisecond) }, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	_, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("idle stream not aborted once: %v, %d", err, attempts.Load())
	}
}

func TestRetryAfterAndPermanentClassification(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if retryAfter("2", now) != 2*time.Second || retryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now) != 3*time.Second || retryAfter("invalid", now) != 0 {
		t.Fatal("invalid Retry-After parsing")
	}
	for _, code := range []string{"insufficient_quota", "billing_hard_limit_reached", "billing_not_active", "invalid_api_key", "account_deactivated"} {
		if retryableStatus(429, errorMetadata{Code: code}) {
			t.Errorf("permanent code retried: %s", code)
		}
	}
	for _, row := range []struct {
		message   string
		hint      string
		permanent bool
	}{
		{"Too many concurrent requests", "concurrency", false},
		{"Rate limit on tokens per minute", "token_rate", false},
		{"Quota for requests per minute exceeded", "request_rate", false},
		{"Service is overloaded", "capacity", false},
		{"You exceeded your current quota", "quota", true},
		{"Unknown upstream problem", "", false},
	} {
		data, _ := json.Marshal(map[string]any{"error": map[string]string{"message": row.message}})
		metadata := describeError(data)
		if metadata.Hint != row.hint || permanentError(metadata) != row.permanent {
			t.Errorf("classification of %q: %+v", row.message, metadata)
		}
	}
}
