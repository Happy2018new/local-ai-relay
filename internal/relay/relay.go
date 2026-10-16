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
	id       string
	attempts int
	queueMS  int64
	outcome  string
}

func (p *Relay) handle(destination http.ResponseWriter, r *http.Request, route string) {
	started := time.Now()
	state := requestState{id: "relay-" + rand.Text(), outcome: "completed"}
	w := &trackedWriter{ResponseWriter: destination}
	w.Header().Set("X-Relay-Request-Id", state.id)
	p.counts.requests.Add(1)
	defer func() {
		status := w.status
		if r.Context().Err() != nil {
			state.outcome = "canceled"
			p.counts.canceled.Add(1)
			if status == 0 {
				status = 499
			}
		} else if status >= 200 && status < 300 && state.outcome == "completed" {
			p.counts.succeeded.Add(1)
		} else {
			p.counts.failed.Add(1)
		}
		p.logger.Info("request_finished", "request_id", state.id, "route", route, "status", status,
			"attempts", state.attempts, "queue_ms", state.queueMS, "duration_ms", time.Since(started).Milliseconds(),
			"bytes_out", w.bytes, "outcome", state.outcome)
	}()
	waitCtx, stopWaiting := context.WithTimeout(r.Context(), p.cfg.QueueTimeout.Value())
	defer stopWaiting()
	select {
	case p.admission <- struct{}{}:
		defer func() { <-p.admission }()
	default:
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
		body, invalid = readRequest(r, p.cfg.MaxRequestBytes, p.cfg.RequireStream)
		_ = controller.SetReadDeadline(time.Time{})
		if invalid != nil {
			localError(w, invalid.status, invalid.code, invalid.message)
			return
		}
	}
	release, err := p.schedule.acquire(waitCtx)
	if err != nil {
		p.queueError(w, r, err)
		return
	}
	defer release()
	if err := p.schedule.waitAttempt(waitCtx); err != nil {
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
		ctx, cancel := context.WithCancel(r.Context())
		upstream := *p.upstream
		upstream.Path = strings.TrimRight(upstream.Path, "/") + route
		upstream.RawQuery = r.URL.RawQuery
		request, err := http.NewRequestWithContext(ctx, r.Method, upstream.String(), bytes.NewReader(body))
		if err != nil {
			cancel()
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
		headerTimer := time.AfterFunc(p.cfg.ResponseHeaderTimeout.Value(), cancel)
		response, err := p.client.Do(request)
		headerTimer.Stop()
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				return
			}
			p.recordError(0, errorMetadata{Code: "upstream_transport_error"}, "")
			p.logger.Warn("upstream_attempt", "request_id", state.id, "attempt", attempt,
				"status", 0, "error_code", "upstream_transport_error", "duration_ms", time.Since(attemptStarted).Milliseconds())
			if !p.prepareRetry(r.Context(), attempt, retryDeadline, backoff(attempt, p.cfg.InitialBackoff.Value(), p.cfg.MaxBackoff.Value())) {
				if r.Context().Err() == nil {
					localError(w, http.StatusBadGateway, "upstream_transport_error", "The upstream connection failed before a response could be forwarded.")
				}
				return
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if route == "/responses" && p.cfg.RequireStream && mediaType != "text/event-stream" {
				response.Body.Close()
				cancel()
				p.recordError(response.StatusCode, errorMetadata{Code: "upstream_not_streaming"}, response.Header.Get("X-Request-Id"))
				localError(w, http.StatusBadGateway, "upstream_not_streaming", "The upstream returned a successful non-SSE response despite stream: true.")
				return
			}
			p.forward(w, response, cancel, &state)
			return
		}
		timer := time.AfterFunc(p.cfg.ErrorBodyTimeout.Value(), cancel)
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
		timer.Stop()
		if readErr != nil {
			response.Body.Close()
			cancel()
			p.recordError(response.StatusCode, errorMetadata{Code: "upstream_error_read_failed"}, response.Header.Get("X-Request-Id"))
			if r.Context().Err() == nil {
				localError(w, http.StatusBadGateway, "upstream_error_read_failed", "Could not read the upstream error within the configured error body timeout.")
			}
			return
		}
		metadata := describeError(data)
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
		p.logger.Warn("upstream_attempt", "request_id", state.id, "attempt", attempt,
			"status", response.StatusCode, "upstream_request_id", safeIdentifier(upstreamID),
			"error_code", metadata.Code, "error_type", metadata.Type, "error_hint", metadata.Hint, "retryable", mayRetry,
			"duration_ms", time.Since(attemptStarted).Milliseconds(), "retry_delay_ms", delay.Milliseconds())
		if mayRetry && attempt < p.cfg.MaxAttempts && time.Now().Add(delay).Before(retryDeadline) {
			response.Body.Close()
			cancel()
			last := &savedResponse{status: response.StatusCode, header: response.Header.Clone(), body: data}
			if p.prepareRetry(r.Context(), attempt, retryDeadline, delay) {
				continue
			}
			if r.Context().Err() != nil {
				return
			}
			p.forward(w, last.response(), func() {}, &state)
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

func (p *Relay) forward(w *trackedWriter, response *http.Response, cancel context.CancelFunc, state *requestState) {
	defer response.Body.Close()
	defer cancel()
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
			panic(http.ErrAbortHandler)
		}
	}
	var idle *time.Timer
	if p.cfg.StreamIdleTimeout > 0 {
		idle = time.AfterFunc(p.cfg.StreamIdleTimeout.Value(), cancel)
		defer idle.Stop()
	}
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if idle != nil {
				idle.Reset(p.cfg.StreamIdleTimeout.Value())
				_ = controller.SetWriteDeadline(time.Now().Add(p.cfg.StreamIdleTimeout.Value()))
			}
			if _, err := w.Write(buffer[:n]); err != nil {
				state.outcome = "downstream_write_error"
				panic(http.ErrAbortHandler)
			}
			if mediaType == "text/event-stream" {
				if err := controller.Flush(); err != nil {
					state.outcome = "downstream_write_error"
					panic(http.ErrAbortHandler)
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				state.outcome = "upstream_stream_error"
				p.logger.Warn("stream_aborted", "request_id", state.id, "attempts", state.attempts)
				panic(http.ErrAbortHandler)
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
