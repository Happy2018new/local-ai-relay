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

type requestMetadata struct {
	Model     string `json:"model"`
	Stream    *bool  `json:"stream"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func readRequest(r *http.Request, limit int64, requireStream bool) ([]byte, requestMetadata, *requestError) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, requestMetadata{}, &requestError{http.StatusBadRequest, "request_read_failed", "Could not read the request body."}
	}
	if int64(len(raw)) > limit {
		return nil, requestMetadata{}, &requestError{http.StatusRequestEntityTooLarge, "request_too_large", "The encoded request exceeds max_request_bytes."}
	}
	metadata, invalid := inspectRequest(raw, r.Header.Get("Content-Encoding"), limit)
	if invalid != nil {
		if !requireStream {
			return raw, requestMetadata{}, nil
		}
		return nil, metadata, invalid
	}
	if requireStream && (metadata.Stream == nil || !*metadata.Stream) {
		return nil, metadata, &requestError{http.StatusBadRequest, "stream_required", "Set stream to true for Responses requests."}
	}
	return raw, metadata, nil
}

func inspectRequest(raw []byte, encoding string, limit int64) (requestMetadata, *requestError) {
	var decoded io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(decoded)
		if err != nil {
			return requestMetadata{}, &requestError{http.StatusBadRequest, "invalid_compression", "The gzip request is invalid."}
		}
		defer reader.Close()
		decoded = reader
	case "zstd":
		reader, err := zstd.NewReader(decoded, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max(limit, 8<<20))))
		if err != nil {
			return requestMetadata{}, &requestError{http.StatusBadRequest, "invalid_compression", "The zstd request is invalid."}
		}
		defer reader.Close()
		decoded = reader
	default:
		return requestMetadata{}, &requestError{http.StatusUnsupportedMediaType, "unsupported_compression", "Use an uncompressed, gzip, or zstd request."}
	}
	data, err := io.ReadAll(io.LimitReader(decoded, limit+1))
	if err != nil {
		return requestMetadata{}, &requestError{http.StatusBadRequest, "invalid_compression", "Could not decode the request body."}
	}
	if int64(len(data)) > limit {
		return requestMetadata{}, &requestError{http.StatusRequestEntityTooLarge, "request_too_large", "The decoded request exceeds max_request_bytes."}
	}
	var envelope requestMetadata
	if err := json.Unmarshal(data, &envelope); err != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return requestMetadata{}, &requestError{http.StatusBadRequest, "invalid_json", "The request must be a JSON object."}
	}
	envelope.Model = safeIdentifier(envelope.Model)
	envelope.Reasoning.Effort = safeIdentifier(envelope.Reasoning.Effort)
	return envelope, nil
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
