package tui

import (
	"encoding/json"
	"strings"
)

// A request keeps its original position and identity while its latest status changes.
func groupLogEntries(lines []string) []logEntry {
	var entries []logEntry
	requests := map[string]int{}
	for _, raw := range lines {
		var data map[string]any
		_ = json.Unmarshal([]byte(raw), &data)
		event := logEntry{raw: raw, data: data}
		requestID := textValue(data["request_id"])
		if requestID == "" {
			entries = append(entries, event)
			continue
		}
		id := "request\x00" + textValue(data["session_id"]) + "\x00" + requestID
		index, exists := requests[id]
		if !exists {
			index = len(entries)
			requests[id] = index
			entries = append(entries, logEntry{id: id, data: map[string]any{}})
		}
		entry := &entries[index]
		entry.events = append(entry.events, event)
		entry.raw = raw
		// Diagnostics describe the latest event; earlier failures stay in its timeline.
		for _, key := range []string{"reason", "reason_code", "error_code", "error_type", "error_hint", "will_retry", "retryable", "retry_delay_ms"} {
			delete(entry.data, key)
		}
		for key, value := range data {
			if (value == "" || value == "unknown") && entry.data[key] != nil {
				continue
			}
			entry.data[key] = value
		}
	}
	return entries
}

func (e logEntry) matches(query string, warnings bool) bool {
	events := e.events
	if len(events) == 0 {
		events = []logEntry{e}
	}
	levelMatches, queryMatches := !warnings, query == ""
	query = strings.ToLower(query)
	for _, event := range events {
		if event.data["level"] == "WARN" || event.data["level"] == "ERROR" {
			levelMatches = true
		}
		if strings.Contains(strings.ToLower(event.raw+" "+eventLabel(event.data)), query) {
			queryMatches = true
		}
	}
	return levelMatches && queryMatches
}
