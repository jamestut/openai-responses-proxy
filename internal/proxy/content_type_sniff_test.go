package proxy

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestMissingContentTypeSSERecordsTerminalUsageAndForwardsBytes(t *testing.T) {
	body := "event: response.completed\ndata: {\"response\":{\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}}\n\n"
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, body)
	}), nil)

	response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	if got := responseBody(t, response); got != body {
		t.Fatalf("forwarded body differs: got %q, want %q", got, body)
	}
	got := waitRecords(t, h, 1)
	want := storedRecord{user: "alice", httpStatus: http.StatusOK, status: "completed", outcome: "completed", input: 7, output: 5, total: 12}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accounting records = %+v, want one %+v", got, want)
	}
}

func TestPlainTextSSERecordsTerminalUsageAndForwardsBytes(t *testing.T) {
	body := "event: response.completed\ndata: {\"response\":{\"object\":\"response\",\"status\":\"completed\",\"model\":\"upstream-model\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}}\n\n"
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}), nil)

	response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{"model":"request-model","stream":true}`, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("downstream HTTP status = %d, want 200", response.StatusCode)
	}
	if got := responseBody(t, response); got != body {
		t.Fatalf("forwarded body differs: got %q, want %q", got, body)
	}
	got := waitRecords(t, h, 1)
	want := storedRecord{
		user: "alice", model: "request-model", httpStatus: http.StatusOK,
		status: "completed", outcome: "completed", input: 7, output: 5, total: 12,
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accounting records = %+v, want one %+v", got, want)
	}
}

func TestMislabeledSSEForwardsBeforeUpstreamEOF(t *testing.T) {
	first := "event: response.in_progress\ndata: {\"response\":{\"object\":\"response\",\"status\":\"in_progress\"}}\n\n"
	last := "event: response.completed\ndata: {\"response\":{\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, first)
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, last)
	}), nil)

	response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	gotFirst := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, gotFirst); err != nil {
		_ = response.Body.Close()
		t.Fatalf("read first SSE frame before upstream EOF: %v", err)
	}
	if string(gotFirst) != first {
		_ = response.Body.Close()
		t.Fatalf("first forwarded bytes = %q, want %q", gotFirst, first)
	}
	releaseOnce.Do(func() { close(release) })
	remaining, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read remaining response: %v", err)
	}
	if string(remaining) != last {
		t.Fatalf("remaining forwarded bytes = %q, want %q", remaining, last)
	}
	got := waitRecords(t, h, 1)
	want := storedRecord{
		user: "alice", httpStatus: http.StatusOK, status: "completed", outcome: "completed",
		input: 2, output: 1, total: 3,
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accounting records = %+v, want one %+v", got, want)
	}
}

func TestPlainTextJSONDetection(t *testing.T) {
	for _, tc := range []struct {
		name, body, model, status string
		input, output, total      int64
	}{
		{
			name:  "object after leading whitespace",
			body:  " \n\t{\"object\":\"response\",\"status\":\"completed\",\"model\":\"json-model\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}",
			model: "json-model", status: "completed", input: 3, output: 4, total: 7,
		},
		{
			name: "array is forwarded and invalid for response accounting",
			body: " [1,2,3]", status: "", model: "", input: 0, output: 0, total: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}), nil)
			response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
			if got := responseBody(t, response); got != tc.body {
				t.Fatalf("forwarded body = %q, want %q", got, tc.body)
			}
			got := waitRecords(t, h, 1)
			want := storedRecord{
				user: "alice", model: tc.model, httpStatus: http.StatusOK,
				status: tc.status, outcome: "completed", input: tc.input, output: tc.output, total: tc.total,
			}
			if tc.name == "array is forwarded and invalid for response accounting" {
				want.outcome = "incomplete"
				want.reason = "invalid_response"
			}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("accounting records = %+v, want one %+v", got, want)
			}
		})
	}
}

func TestBodyDetectionFallsBackToJSONAtFourKiBWhitespaceLimit(t *testing.T) {
	// An SSE-looking body beyond the bounded lookahead falls back to JSON, which cannot extract usage.
	body := strings.Repeat(" ", 4<<10) + "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}), nil)
	response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	if got := responseBody(t, response); got != body {
		t.Fatalf("forwarded body differs: got %d bytes, want %d bytes", len(got), len(body))
	}
	got := waitRecords(t, h, 1)
	want := storedRecord{user: "alice", httpStatus: http.StatusOK, outcome: "incomplete", reason: "invalid_response"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("accounting records = %+v, want one %+v", got, want)
	}
}

func TestBodyDetectionEmptyWhitespaceAndReadError(t *testing.T) {
	t.Run("empty response uses JSON fallback regardless of content type", func(t *testing.T) {
		h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
		}), nil)
		response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
		if got := responseBody(t, response); got != "" {
			t.Fatalf("forwarded body = %q, want empty", got)
		}
		got := waitRecords(t, h, 1)
		want := storedRecord{user: "alice", httpStatus: http.StatusOK, outcome: "incomplete", reason: "invalid_response"}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("accounting records = %+v, want one %+v", got, want)
		}
	})
	t.Run("whitespace response uses JSON fallback regardless of content type", func(t *testing.T) {
		body := " \n\t "
		h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}), nil)
		response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
		if got := responseBody(t, response); got != body {
			t.Fatalf("forwarded body = %q, want %q", got, body)
		}
		got := waitRecords(t, h, 1)
		want := storedRecord{user: "alice", httpStatus: http.StatusOK, outcome: "incomplete", reason: "invalid_response"}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("accounting records = %+v, want one %+v", got, want)
		}
	})

	t.Run("SSE read error is preserved after detection", func(t *testing.T) {
		body := "event: response.in_progress\ndata: {\"response\":{\"object\":\"response\",\"status\":\"in_progress\"}}\n"
		h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+20))
			_, _ = io.WriteString(w, body)
		}), nil)
		response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
		forwarded, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("downstream HTTP status = %d, want 200", response.StatusCode)
		}
		if readErr == nil {
			t.Error("downstream body read error = nil, want truncated-body error")
		}
		if string(forwarded) != body {
			t.Errorf("forwarded body = %q, want %q", forwarded, body)
		}
		got := waitRecords(t, h, 1)
		want := storedRecord{user: "alice", httpStatus: http.StatusOK, outcome: "incomplete", reason: "response_read_error"}
		if len(got) != 1 || got[0] != want {
			t.Errorf("accounting records = %+v, want one %+v", got, want)
		}
	})

	t.Run("read error while lookahead contains only whitespace", func(t *testing.T) {
		body := "  \n\t "
		h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+20))
			_, _ = io.WriteString(w, body)
		}), nil)
		response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
		forwarded, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr == nil {
			t.Error("downstream body read error = nil, want truncated-body error")
		}
		if string(forwarded) != body {
			t.Errorf("forwarded body = %q, want %q", forwarded, body)
		}
		got := waitRecords(t, h, 1)
		want := storedRecord{user: "alice", httpStatus: http.StatusOK, outcome: "incomplete", reason: "response_read_error"}
		if len(got) != 1 || got[0] != want {
			t.Errorf("accounting records = %+v, want one %+v", got, want)
		}
	})
}

func TestBodyDetectionOverridesContentTypeAndHandlesErrors(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
		wantStatus, wantOutcome string
		input, output           int64
	}{
		{
			name: "standard event stream", contentType: "text/event-stream",
			body:   "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"model\":\"sse-model\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n",
			status: http.StatusOK, wantStatus: "completed", wantOutcome: "completed", input: 1, output: 2,
		},
		{
			name: "SSE body labeled as JSON", contentType: "application/json",
			body:   "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"model\":\"sse-model\",\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n",
			status: http.StatusOK, wantStatus: "completed", wantOutcome: "completed", input: 4, output: 3,
		},
		{
			name: "JSON body labeled as event stream", contentType: "text/event-stream",
			body:   `{"status":"completed","model":"error-model","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`,
			status: http.StatusOK, wantStatus: "completed", wantOutcome: "completed", input: 2, output: 3,
		},
		{
			name: "SSE body with unrelated type on HTTP error", contentType: "application/octet-stream",
			body:   "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"model\":\"sse-model\",\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n",
			status: http.StatusBadGateway, wantStatus: "completed", wantOutcome: "failed", input: 4, output: 3,
		},
		{
			name: "plain text JSON error", contentType: "text/plain",
			body:   `{"status":"failed","model":"error-model","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`,
			status: http.StatusBadRequest, wantStatus: "failed", wantOutcome: "failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), nil)
			response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
			if response.StatusCode != tc.status {
				t.Fatalf("downstream HTTP status = %d, want %d", response.StatusCode, tc.status)
			}
			if got := responseBody(t, response); got != tc.body {
				t.Fatalf("forwarded body = %q, want %q", got, tc.body)
			}
			got := waitRecords(t, h, 1)
			want := storedRecord{
				user: "alice", httpStatus: tc.status, status: tc.wantStatus,
				outcome: tc.wantOutcome, input: tc.input, output: tc.output, total: tc.input + tc.output,
			}
			if strings.Contains(tc.body, "event:") {
				want.model = "sse-model"
			} else {
				want.model = "error-model"
			}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("accounting records = %+v, want one %+v", got, want)
			}
		})
	}
}
