package relay

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxErrorBytes = 64 << 10

type errorMetadata struct {
	Code string
	Type string
	Hint string
}

func describeError(body []byte) errorMetadata {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return errorMetadata{}
	}
	return errorMetadata{Code: safeIdentifier(envelope.Error.Code), Type: safeIdentifier(envelope.Error.Type), Hint: errorHint(envelope.Error.Message)}
}

func errorHint(message string) string {
	message = strings.ToLower(message)
	for _, group := range []struct {
		hint    string
		phrases []string
	}{
		{"quota", []string{"insufficient_quota", "exceeded your current quota", "insufficient balance", "credit balance is too low", "out of credits", "billing hard limit"}},
		{"concurrency", []string{"concurrent", "concurrency", "in-flight limit"}},
		{"token_rate", []string{"tokens per min", "tokens_per_min", "token rate", "tpm limit"}},
		{"request_rate", []string{"requests per min", "requests_per_min", "request rate", "rpm limit"}},
		{"capacity", []string{"overloaded", "at capacity", "no available channels", "no available upstream"}},
	} {
		for _, phrase := range group.phrases {
			if strings.Contains(message, phrase) {
				return group.hint
			}
		}
	}
	return ""
}

// Log only short machine identifiers; upstream messages can echo prompts or keys.
func safeIdentifier(value string) string {
	if len(value) > 128 {
		return "redacted"
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return "redacted"
		}
	}
	return value
}

func permanentError(metadata errorMetadata) bool {
	if metadata.Hint == "quota" {
		return true
	}
	for _, value := range []string{metadata.Code, metadata.Type} {
		switch strings.ToLower(value) {
		case "insufficient_quota", "billing_hard_limit_reached", "billing_not_active", "invalid_api_key", "account_deactivated":
			return true
		}
	}
	return false
}

func retryableStatus(status int, metadata errorMetadata) bool {
	if permanentError(metadata) {
		return false
	}
	switch status {
	case 408, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if seconds, err := strconv.ParseUint(header, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(header); err == nil && until.After(now) {
		return until.Sub(now)
	}
	return 0
}

func backoff(attempt int, initial, maximum time.Duration) time.Duration {
	value := initial
	for i := 1; i < attempt && value < maximum; i++ {
		if value > maximum/2 {
			value = maximum
			break
		}
		value *= 2
	}
	value = min(value, maximum)
	half := value / 2
	return half + time.Duration(rand.Int64N(max(int64(value-half), 1)))
}

func waitUntil(ctx context.Context, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
