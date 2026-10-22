package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise an invalid key, an upstream 500, and a 429 followed by success.
// Default logs must include sanitized error/retry details and omit sensitive markers.
func TestHTTPErrorAndRetryLogsContainOnlySanitizedMetadata(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The invalid client key must not consume a response from this sequence.
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"response-body-marker"}}`)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_, _ = io.WriteString(w, `{"object":"response","status":"completed"}`)
		}
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	unauthorized := h.do(t, http.MethodGet, "/v1/responses", "unknown-test-key", "", nil)
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown key status = %d, want 401", unauthorized.StatusCode)
	}
	errorResponse := h.do(t, http.MethodGet, "/v1/responses/path-id-marker?query-marker=true", "alice-test-key", "", nil)
	_ = responseBody(t, errorResponse)
	if errorResponse.StatusCode != http.StatusInternalServerError {
		t.Errorf("upstream error status = %d, want 500", errorResponse.StatusCode)
	}
	retryResponse := h.do(t, http.MethodPost, "/v1/responses?query-marker=true", "alice-test-key", `{"model":"m","input":"request-body-marker"}`, nil)
	_ = responseBody(t, retryResponse)
	if retryResponse.StatusCode != http.StatusOK || calls.Load() != 3 {
		t.Errorf("retry status = %d, upstream attempts = %d; want 200, 3 total attempts", retryResponse.StatusCode, calls.Load())
	}
	logs, err := os.ReadFile(h.logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Distinct markers reveal leaks from credentials, resource IDs, queries, or bodies.
	for _, marker := range []string{
		"alice-test-key", "unknown-test-key", "upstream-test-secret", "path-id-marker", "query-marker", "request-body-marker", "response-body-marker",
	} {
		if strings.Contains(string(logs), marker) {
			t.Error("log contains a forbidden synthetic credential, identifier, query, or body marker")
		}
	}
	// Checking structured fields proves the logs remain useful after sanitization.
	var foundHTTPError, foundRetry bool
	scanner := bufio.NewScanner(strings.NewReader(string(logs)))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal("log entry is not valid JSON")
		}
		switch entry["msg"] {
		case "upstream returned an HTTP error":
			foundHTTPError = true
			if entry["status"] != float64(500) || entry["method"] != "GET" || entry["path"] != "/v1/responses/{response_id}" || entry["category"] != "server_error" {
				t.Error("HTTP error log lacks the expected sanitized status, method, route, or category")
			}
		case "upstream returned 429; retrying":
			foundRetry = true
			if entry["level"] != "WARN" || entry["retry"] != float64(1) {
				t.Error("retry log lacks warning severity or retry number 1")
			}
		case "downstream authentication failed":
			t.Error("default upstream_errors logging included downstream authentication failure")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundHTTPError || !foundRetry {
		t.Errorf("HTTP error log present = %v, retry log present = %v; want both true", foundHTTPError, foundRetry)
	}
}
