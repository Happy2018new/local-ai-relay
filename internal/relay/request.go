package relay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type requestError struct {
	status  int
	code    string
	message string
}

func readRequest(r *http.Request, limit int64, requireStream bool) ([]byte, *requestError) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, &requestError{http.StatusBadRequest, "request_read_failed", "Could not read the request body."}
	}
	if int64(len(raw)) > limit {
		return nil, &requestError{http.StatusRequestEntityTooLarge, "request_too_large", "The encoded request exceeds max_request_bytes."}
	}
	if !requireStream {
		return raw, nil
	}
	var decoded io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(decoded)
		if err != nil {
			return nil, &requestError{http.StatusBadRequest, "invalid_compression", "The gzip request is invalid."}
		}
		defer reader.Close()
		decoded = reader
	case "zstd":
		reader, err := zstd.NewReader(decoded, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max(limit, 8<<20))))
		if err != nil {
			return nil, &requestError{http.StatusBadRequest, "invalid_compression", "The zstd request is invalid."}
		}
		defer reader.Close()
		decoded = reader
	default:
		return nil, &requestError{http.StatusUnsupportedMediaType, "unsupported_compression", "Use an uncompressed, gzip, or zstd request."}
	}
	data, err := io.ReadAll(io.LimitReader(decoded, limit+1))
	if err != nil {
		return nil, &requestError{http.StatusBadRequest, "invalid_compression", "Could not decode the request body."}
	}
	if int64(len(data)) > limit {
		return nil, &requestError{http.StatusRequestEntityTooLarge, "request_too_large", "The decoded request exceeds max_request_bytes."}
	}
	var envelope struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, &requestError{http.StatusBadRequest, "invalid_json", "The request must be a JSON object."}
	}
	if envelope.Stream == nil || !*envelope.Stream {
		return nil, &requestError{http.StatusBadRequest, "stream_required", "Set stream to true for Responses requests."}
	}
	return raw, nil
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(field))
		}
	}
	for _, field := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(field)
	}
}

func copyHeaders(destination, source http.Header) {
	clean := source.Clone()
	stripHopHeaders(clean)
	for key, values := range clean {
		destination[key] = append([]string(nil), values...)
	}
}

type joinedBody struct {
	io.Reader
	io.Closer
}
