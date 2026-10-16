package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
)

func TestConcurrentClientsStayWithinUpstreamLimit(t *testing.T) {
	var active, peak atomic.Int32
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": first chunk\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(15 * time.Millisecond)
		fmt.Fprint(w, completeEvent)
	}, func(c *config.Config) { c.MaxConcurrent = 2 }, config.Secrets{}, nil)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
			if bodyOf(t, response) != ": first chunk\n\n"+completeEvent {
				t.Error("stream changed")
			}
		}()
	}
	group.Wait()
	if peak.Load() != 2 {
		t.Fatalf("upstream peak=%d want=2", peak.Load())
	}
	eventually(t, func() bool { return p.schedule.status().Active == 0 && p.counts.succeeded.Load() == 12 })
}

func TestQueueFullIsLocalAndHealthRemainsAvailable(t *testing.T) {
	p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, func(c *config.Config) { c.MaxQueue = 0 }, config.Secrets{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(streamRequest))
	first, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	if got := bodyOf(t, response); response.StatusCode != 503 || !strings.Contains(got, "relay_queue_full") {
		t.Fatalf("queue response: %d %s", response.StatusCode, got)
	}
	health, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	bodyOf(t, health)
	if health.StatusCode != 200 || p.counts.queueRejected.Load() != 1 {
		t.Fatal("health blocked or queue rejection not recorded")
	}
}

func TestModelsAndBasePathMapping(t *testing.T) {
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/v1/models" || r.URL.RawQuery != "probe=1" || r.Method != http.MethodGet {
			t.Errorf("incorrect upstream route: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}, func(c *config.Config) { c.UpstreamURL += "/custom/v1/" }, config.Secrets{}, nil)
	response, err := server.Client().Get(server.URL + "/v1/models?probe=1")
	if err != nil {
		t.Fatal(err)
	}
	if got := bodyOf(t, response); got != `{"data":[]}` || response.StatusCode != 200 {
		t.Fatalf("models failed: %d %s", response.StatusCode, got)
	}
}

func TestTransportFailureBeforeResponseCanRetry(t *testing.T) {
	var attempts atomic.Int32
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		writeStream(w)
	}, nil, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	if got := bodyOf(t, response); got != completeEvent || attempts.Load() != 2 {
		t.Fatalf("transport retry failed: attempts=%d body=%s", attempts.Load(), got)
	}
}

func TestErrorBodyTimeoutIsBounded(t *testing.T) {
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, func(c *config.Config) { c.ErrorBodyTimeout = config.Duration(30 * time.Millisecond) }, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	if got := bodyOf(t, response); response.StatusCode != 502 || !strings.Contains(got, "upstream_error_read_failed") {
		t.Fatalf("error body timeout not enforced: %d %s", response.StatusCode, got)
	}
}

func TestOversizedUpstreamErrorIsReturnedWithoutTruncation(t *testing.T) {
	original := strings.Repeat("x", maxErrorBytes+1024)
	var attempts atomic.Int32
	_, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(429)
		io.WriteString(w, original)
	}, nil, config.Secrets{}, nil)
	response := post(t, server.Client(), server.URL, []byte(streamRequest), nil)
	if got := bodyOf(t, response); got != original || attempts.Load() != 1 {
		t.Fatal("large error was truncated or replayed")
	}
}
