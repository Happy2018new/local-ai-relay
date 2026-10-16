package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
	"github.com/klauspost/compress/zstd"
)

// Opt-in: run an isolated Codex client against a fake upstream, with no API key.
func TestCodexClient(t *testing.T) {
	bin := os.Getenv("LOCAL_AI_RELAY_CODEX_BIN")
	if bin == "" {
		t.Skip("set LOCAL_AI_RELAY_CODEX_BIN to opt in to the local Codex smoke test")
	}
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression_%t", compressed), func(t *testing.T) {
			var attempts atomic.Int32
			var encoding atomic.Value
			p, server := testRelay(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Errorf("unexpected upstream route: %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				encoding.Store(r.Header.Get("Content-Encoding"))
				var reader io.Reader = r.Body
				if r.Header.Get("Content-Encoding") == "zstd" {
					decoder, err := zstd.NewReader(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer decoder.Close()
					reader = decoder
				}
				var request struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(reader).Decode(&request); err != nil || !request.Stream {
					t.Errorf("Codex did not send a streaming request: %v", err)
					w.WriteHeader(400)
					return
				}
				if attempts.Add(1) == 1 {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"Local smoke test"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				item := map[string]any{
					"id": "msg_local_smoke", "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}},
				}
				for _, event := range []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "resp_local_smoke", "status": "in_progress", "output": []any{}}},
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_local_smoke", "type": "message", "role": "assistant", "content": []any{}}},
					{"type": "response.output_text.delta", "item_id": "msg_local_smoke", "output_index": 0, "content_index": 0, "delta": "OK"},
					{"type": "response.output_item.done", "output_index": 0, "item": item},
					{"type": "response.completed", "response": map[string]any{"id": "resp_local_smoke", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
				} {
					data, _ := json.Marshal(event)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
					w.(http.Flusher).Flush()
				}
			}, nil, config.Secrets{}, nil)
			provider := `model_providers.relaysmoke={name="Local relay smoke",base_url=` + strconv.Quote(server.URL) + `,wire_api="responses",requires_openai_auth=false,supports_websockets=false,request_max_retries=0,stream_max_retries=0,capabilities={remote_compaction="unsupported"}}`
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "--no-daemon", "exec", "--ignore-user-config", "--ignore-rules", "--strict-config", "--ephemeral", "--skip-git-repo-check",
				"--color", "never", "--json", "-s", "read-only", "-C", t.TempDir(),
				"-c", `model_provider="relaysmoke"`, "-c", `model="gpt-6.1-sol"`,
				"-c", "features.enable_request_compression="+strconv.FormatBool(compressed), "-c", provider,
				"Return OK. This is a local relay smoke test.")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("isolated Codex failed: %v\n%s", err, output)
			}
			if attempts.Load() != 2 || !strings.Contains(string(output), "OK") || strings.Contains(strings.ToLower(string(output)), "unknown field") {
				t.Fatalf("unexpected Codex smoke result: attempts=%d\n%s", attempts.Load(), output)
			}
			t.Logf("Codex completed; attempts=%d, retries=%d, stream=true, content_encoding=%v", attempts.Load(), p.counts.retries.Load(), encoding.Load())
		})
	}
}
