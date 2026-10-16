package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
)

type counters struct {
	requests      atomic.Int64
	succeeded     atomic.Int64
	failed        atomic.Int64
	canceled      atomic.Int64
	attempts      atomic.Int64
	retries       atomic.Int64
	rateLimited   atomic.Int64
	queueRejected atomic.Int64
}

type LastError struct {
	Time              time.Time `json:"time"`
	Status            int       `json:"status"`
	Code              string    `json:"code,omitempty"`
	Type              string    `json:"type,omitempty"`
	Hint              string    `json:"hint,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
}

type Relay struct {
	cfg       config.Config
	secrets   config.Secrets
	upstream  *url.URL
	client    *http.Client
	schedule  *scheduler
	admission chan struct{}
	logger    *slog.Logger
	started   time.Time
	counts    counters
	errorMu   sync.Mutex
	lastError *LastError
}

func New(cfg config.Config, secrets config.Secrets, logger *slog.Logger) (*Relay, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if secrets.LocalKey != "" && secrets.UpstreamKey == "" {
		return nil, errors.New("local authentication requires a separate upstream key")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	upstream, _ := url.Parse(cfg.UpstreamURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout.Value()
	transport.MaxIdleConnsPerHost = max(cfg.MaxConcurrent, 2)
	return &Relay{
		cfg: cfg, secrets: secrets, upstream: upstream, logger: logger, started: time.Now(),
		schedule:  newScheduler(cfg.MaxConcurrent, cfg.MaxQueue, cfg.MinInterval.Value()),
		admission: make(chan struct{}, cfg.MaxConcurrent+cfg.MaxQueue),
		client:    &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (p *Relay) Close() { p.client.CloseIdleConnections() }

func (p *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if p.secrets.LocalKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.secrets.LocalKey)) != 1 {
		localError(w, http.StatusUnauthorized, "invalid_local_key", "A valid local relay bearer token is required.")
		return
	}
	if r.URL.Path == "/status" && r.Method == http.MethodGet {
		p.serveStatus(w)
		return
	}
	var route string
	switch r.URL.Path {
	case "/responses", "/v1/responses":
		route = "/responses"
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			localError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Responses requires POST.")
			return
		}
	case "/models", "/v1/models":
		route = "/models"
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			localError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Models requires GET.")
			return
		}
	default:
		localError(w, http.StatusNotFound, "route_not_found", "Use /responses, /models, /healthz, or /status.")
		return
	}
	p.handle(w, r, route)
}

type requestState struct {
	id                         string
	attempts                   int
	queueMS                    int64
	outcome                    string
	route                      string
	started                    time.Time
	downstream                 context.Context
	upstreamCtx                context.Context
	metadata                   requestMetadata
	upstreamSSE                *bool
	firstByteMS, firstOutputMS *int64
	usage                      *TokenUsage
	reasonCode, reason         string
}

func (p *Relay) handle(destination http.ResponseWriter, r *http.Request, route string) {
	started := time.Now()
	state := requestState{id: "relay-" + rand.Text(), outcome: "completed", route: route, started: started, downstream: r.Context()}
	w := &trackedWriter{ResponseWriter: destination}
	w.Header().Set("X-Relay-Request-Id", state.id)
	p.counts.requests.Add(1)
	defer func() {
		status := w.status
		level := slog.LevelInfo
		if r.Context().Err() != nil {
			state.outcome = "canceled"
			state.reasonCode, state.reason = "request_canceled", "客户端取消了请求或代理正在停止"
			p.counts.canceled.Add(1)
			if status == 0 {
				status = 499
			}
		} else if status >= 200 && status < 300 && state.outcome == "completed" {
			p.counts.succeeded.Add(1)
		} else {
			p.counts.failed.Add(1)
			level = slog.LevelWarn
			if status >= 500 || state.outcome != "completed" {
				level = slog.LevelError
			}
		}
		if level != slog.LevelInfo && state.reason == "" {
			state.reason = localReason(state.reasonCode)
		}
		p.logRequest(&state, level, "request_finished", "status", status,
			"attempts", state.attempts, "queue_ms", state.queueMS,
			"bytes_out", w.bytes, "outcome", state.outcome)
	}()
	waitCtx, stopWaiting := context.WithTimeout(r.Context(), p.cfg.QueueTimeout.Value())
	defer stopWaiting()
	select {
	case p.admission <- struct{}{}:
		defer func() { <-p.admission }()
	default:
		state.reasonCode, state.reason = "relay_queue_full", localReason("relay_queue_full")
		p.queueError(w, r, ErrQueueFull)
		return
	}
	// Finish reading before queueing so net/http can detect a waiting client's disconnect.
	var body []byte
	if route == "/responses" {
		controller := http.NewResponseController(w)
		readDeadline := time.Now().Add(30 * time.Second)
		if queueDeadline, ok := waitCtx.Deadline(); ok && queueDeadline.Before(readDeadline) {
			readDeadline = queueDeadline
		}
		_ = controller.SetReadDeadline(readDeadline)
		var invalid *requestError
		body, state.metadata, invalid = readRequest(r, p.cfg.MaxRequestBytes, p.cfg.RequireStream)
		_ = controller.SetReadDeadline(time.Time{})
		if invalid != nil {
			state.reasonCode, state.reason = invalid.code, localReason(invalid.code)
			localError(w, invalid.status, invalid.code, invalid.message)
			return
		}
	}
	p.logRequest(&state, slog.LevelInfo, "request_received")
	release, err := p.schedule.acquire(waitCtx)
	if err != nil {
		state.reasonCode = "relay_queue_timeout"
		if errors.Is(err, ErrQueueFull) {
			state.reasonCode = "relay_queue_full"
		}
		state.reason = localReason(state.reasonCode)
		p.queueError(w, r, err)
		return
	}
	defer release()
	if err := p.schedule.waitAttempt(waitCtx); err != nil {
		state.reasonCode, state.reason = "relay_queue_timeout", localReason("relay_queue_timeout")
		p.queueError(w, r, err)
		return
	}
	stopWaiting()
	state.queueMS = time.Since(started).Milliseconds()
	retryDeadline := time.Now().Add(p.cfg.RetryWindow.Value())
	for attempt := 1; attempt <= p.cfg.MaxAttempts; attempt++ {
		state.attempts = attempt
		p.counts.attempts.Add(1)
		if attempt > 1 {
			p.counts.retries.Add(1)
		}
		attemptStarted := time.Now()
		ctx, cancel := context.WithCancelCause(r.Context())
		state.upstreamCtx, state.upstreamSSE = ctx, nil
		upstream := *p.upstream
		upstream.Path = strings.TrimRight(upstream.Path, "/") + route
		upstream.RawQuery = r.URL.RawQuery
		request, err := http.NewRequestWithContext(ctx, r.Method, upstream.String(), bytes.NewReader(body))
		if err != nil {
			cancel(nil)
			state.reasonCode, state.reason = "invalid_request", "无法构造上游请求，请检查上游地址和请求格式"
			localError(w, http.StatusBadRequest, "invalid_request", "Could not construct the upstream request.")
			return
		}
		request.Header = r.Header.Clone()
		stripHopHeaders(request.Header)
		request.Header.Set("Accept-Encoding", "identity")
		if p.secrets.UpstreamKey != "" {
			request.Header.Set("Authorization", "Bearer "+p.secrets.UpstreamKey)
		}
		if route == "/responses" && p.cfg.RequireStream {
			request.Header.Set("Accept", "text/event-stream")
		}
		headerTimer := time.AfterFunc(p.cfg.ResponseHeaderTimeout.Value(), func() { cancel(errResponseHeaders) })
		response, err := p.client.Do(request)
		headerTimer.Stop()
		if err != nil {
			state.reasonCode, state.reason = transportReason(err, ctx)
			cancel(nil)
			if r.Context().Err() != nil {
				return
			}
			p.recordError(0, errorMetadata{Code: "upstream_transport_error"}, "")
			delay := backoff(attempt, p.cfg.InitialBackoff.Value(), p.cfg.MaxBackoff.Value())
			willRetry := attempt < p.cfg.MaxAttempts && time.Now().Add(delay).Before(retryDeadline)
			p.logRequest(&state, slog.LevelWarn, "upstream_attempt", "attempt", attempt,
				"status", 0, "error_code", "upstream_transport_error", "attempt_ms", time.Since(attemptStarted).Milliseconds(), "will_retry", willRetry, "retry_delay_ms", delay.Milliseconds())
			if !p.prepareRetry(r.Context(), attempt, retryDeadline, delay) {
				if r.Context().Err() == nil {
					localError(w, http.StatusBadGateway, "upstream_transport_error", "The upstream connection failed before a response could be forwarded.")
				}
				return
			}
			continue
		}
		mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		isSSE := mediaType == "text/event-stream"
		state.upstreamSSE = &isSSE
		state.reasonCode, state.reason = "", ""
		p.logRequest(&state, slog.LevelInfo, "upstream_response", "attempt", attempt, "status", response.StatusCode,
			"response_header_ms", time.Since(attemptStarted).Milliseconds())
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if route == "/responses" && p.cfg.RequireStream && mediaType != "text/event-stream" {
				response.Body.Close()
				cancel(nil)
				state.reasonCode, state.reason = "upstream_not_streaming", "上游返回成功状态，但响应不是 SSE；已按强制流模式拒绝转发"
				p.recordError(response.StatusCode, errorMetadata{Code: "upstream_not_streaming"}, response.Header.Get("X-Request-Id"))
				localError(w, http.StatusBadGateway, "upstream_not_streaming", "The upstream returned a successful non-SSE response despite stream: true.")
				return
			}
			p.forward(w, response, cancel, &state)
			return
		}
		timer := time.AfterFunc(p.cfg.ErrorBodyTimeout.Value(), func() { cancel(context.DeadlineExceeded) })
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
		timer.Stop()
		if readErr != nil {
			response.Body.Close()
			cancel(nil)
			state.reasonCode, state.reason = "upstream_error_read_failed", "读取上游错误响应超时或连接断开，无法取得完整错误信息"
			p.recordError(response.StatusCode, errorMetadata{Code: "upstream_error_read_failed"}, response.Header.Get("X-Request-Id"))
			if r.Context().Err() == nil {
				localError(w, http.StatusBadGateway, "upstream_error_read_failed", "Could not read the upstream error within the configured error body timeout.")
			}
			return
		}
		metadata := describeError(data)
		state.reasonCode, state.reason = metadata.Code, upstreamReason(response.StatusCode, metadata)
		upstreamID := response.Header.Get("X-Request-Id")
		p.recordError(response.StatusCode, metadata, upstreamID)
		mayRetry := retryableStatus(response.StatusCode, metadata) && len(data) <= maxErrorBytes
		delay := max(backoff(attempt, p.cfg.InitialBackoff.Value(), p.cfg.MaxBackoff.Value()), retryAfter(response.Header.Get("Retry-After"), time.Now()))
		if response.StatusCode == http.StatusTooManyRequests {
			p.counts.rateLimited.Add(1)
			if !permanentError(metadata) {
				p.schedule.coolUntil(time.Now().Add(delay))
			}
		}
		willRetry := mayRetry && attempt < p.cfg.MaxAttempts && time.Now().Add(delay).Before(retryDeadline)
		p.logRequest(&state, slog.LevelWarn, "upstream_attempt", "attempt", attempt,
			"status", response.StatusCode, "upstream_request_id", safeIdentifier(upstreamID),
			"error_code", metadata.Code, "error_type", metadata.Type, "error_hint", metadata.Hint, "retryable", mayRetry,
			"attempt_ms", time.Since(attemptStarted).Milliseconds(), "will_retry", willRetry, "retry_delay_ms", delay.Milliseconds())
		if willRetry {
			response.Body.Close()
			cancel(nil)
			last := &savedResponse{status: response.StatusCode, header: response.Header.Clone(), body: data}
			if p.prepareRetry(r.Context(), attempt, retryDeadline, delay) {
				continue
			}
			if r.Context().Err() != nil {
				return
			}
			p.forward(w, last.response(), func(error) {}, &state)
			return
		}
		response.Body = joinedBody{Reader: io.MultiReader(bytes.NewReader(data), response.Body), Closer: response.Body}
		p.forward(w, response, cancel, &state)
		return
	}
}

func (p *Relay) prepareRetry(ctx context.Context, attempt int, deadline time.Time, delay time.Duration) bool {
	if attempt >= p.cfg.MaxAttempts || !time.Now().Add(delay).Before(deadline) {
		return false
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if waitUntil(bounded, time.Now().Add(delay)) != nil {
		return false
	}
	return p.schedule.waitAttempt(bounded) == nil
}

func (p *Relay) queueError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	p.counts.queueRejected.Add(1)
	w.Header().Set("Retry-After", "1")
	if errors.Is(err, ErrQueueFull) {
		localError(w, http.StatusServiceUnavailable, "relay_queue_full", "The local relay queue is full.")
	} else {
		localError(w, http.StatusServiceUnavailable, "relay_queue_timeout", "The request could not start before queue_timeout elapsed.")
	}
}

func (p *Relay) forward(w *trackedWriter, response *http.Response, cancel context.CancelCauseFunc, state *requestState) {
	defer response.Body.Close()
	defer cancel(nil)
	var observedAt time.Time
	observer := streamObserver{
		onOutput: func() {
			if state.firstOutputMS == nil {
				elapsed := observedAt.Sub(state.started).Milliseconds()
				state.firstOutputMS = &elapsed
				p.logRequest(state, slog.LevelInfo, "first_output")
			}
		},
		onUsage: func(usage *TokenUsage) { state.usage = usage; p.logRequest(state, slog.LevelInfo, "token_usage") },
		onFailure: func(metadata errorMetadata) {
			state.outcome = "upstream_response_failed"
			state.reasonCode, state.reason = metadata.Code, generationReason(metadata)
			p.logRequest(state, slog.LevelError, "response_failed", "error_code", metadata.Code)
		},
	}
	copyHeaders(w.Header(), response.Header)
	w.Header().Set("X-Relay-Request-Id", state.id)
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	if p.cfg.StreamIdleTimeout > 0 {
		_ = controller.SetWriteDeadline(time.Now().Add(p.cfg.StreamIdleTimeout.Value()))
		defer controller.SetWriteDeadline(time.Time{})
	}
	if mediaType == "text/event-stream" {
		if err := controller.Flush(); err != nil {
			state.outcome = "downstream_write_error"
			state.reasonCode, state.reason = "downstream_write_error", "无法向客户端刷新响应头，客户端可能已经断开"
			panic(http.ErrAbortHandler)
		}
	}
	var idle *time.Timer
	if p.cfg.StreamIdleTimeout > 0 {
		idle = time.AfterFunc(p.cfg.StreamIdleTimeout.Value(), func() { cancel(errStreamIdle) })
		defer idle.Stop()
	}
	buffer := make([]byte, 32<<10)
	var jsonBody []byte
	jsonTooLarge := false
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			observedAt = time.Now()
			if state.firstByteMS == nil {
				elapsed := time.Since(state.started).Milliseconds()
				state.firstByteMS = &elapsed
			}
			if idle != nil {
				idle.Reset(p.cfg.StreamIdleTimeout.Value())
				_ = controller.SetWriteDeadline(time.Now().Add(p.cfg.StreamIdleTimeout.Value()))
			}
			if _, err := w.Write(buffer[:n]); err != nil {
				state.outcome = "downstream_write_error"
				state.reasonCode, state.reason = "downstream_write_error", "向客户端写入响应失败，客户端可能已经断开或读取超时"
				panic(http.ErrAbortHandler)
			}
			if mediaType == "text/event-stream" {
				if err := controller.Flush(); err != nil {
					state.outcome = "downstream_write_error"
					state.reasonCode, state.reason = "downstream_write_error", "向客户端刷新流数据失败，客户端可能已经断开或读取超时"
					panic(http.ErrAbortHandler)
				}
				observer.feed(buffer[:n])
			} else if mediaType == "application/json" && !jsonTooLarge {
				if len(jsonBody)+n <= maxObservedEventBytes {
					jsonBody = append(jsonBody, buffer[:n]...)
				} else {
					jsonBody = nil
					jsonTooLarge = true
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				state.outcome = "upstream_stream_error"
				state.reasonCode, state.reason = transportReason(readErr, state.upstreamCtx)
				level := slog.LevelWarn
				if state.downstream.Err() != nil {
					level = slog.LevelInfo
					state.reasonCode, state.reason = "request_canceled", "客户端取消了请求或代理正在停止"
				}
				p.logRequest(state, level, "stream_aborted", "attempts", state.attempts)
				panic(http.ErrAbortHandler)
			}
			if mediaType == "text/event-stream" {
				observer.finish()
			} else if !jsonTooLarge {
				observer.observe(jsonBody, "")
			}
			return
		}
	}
}

type savedResponse struct {
	status int
	header http.Header
	body   []byte
}

func (s *savedResponse) response() *http.Response {
	return &http.Response{StatusCode: s.status, Header: s.header, Body: io.NopCloser(bytes.NewReader(s.body))}
}

type trackedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *trackedWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}

func localError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": "relay_error", "code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (p *Relay) recordError(status int, metadata errorMetadata, requestID string) {
	p.errorMu.Lock()
	defer p.errorMu.Unlock()
	p.lastError = &LastError{Time: time.Now().UTC(), Status: status, Code: metadata.Code, Type: metadata.Type, Hint: metadata.Hint, UpstreamRequestID: safeIdentifier(requestID)}
}

func (p *Relay) serveStatus(w http.ResponseWriter) {
	p.errorMu.Lock()
	last := p.lastError
	p.errorMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": time.Since(p.started).Seconds(), "upstream_host": p.upstream.Host,
		"scheduler": p.schedule.status(), "admitted": len(p.admission), "last_error": last,
		"counters": map[string]int64{
			"requests": p.counts.requests.Load(), "succeeded": p.counts.succeeded.Load(),
			"failed": p.counts.failed.Load(), "canceled": p.counts.canceled.Load(),
			"upstream_attempts": p.counts.attempts.Load(), "retries": p.counts.retries.Load(),
			"rate_limited": p.counts.rateLimited.Load(), "queue_rejected": p.counts.queueRejected.Load(),
		},
	})
}
