package proxy

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// End JSON and SSE bodies before their advertised length. The client must receive
// the original bytes and HTTP 200, while accounting marks the read as incomplete.
func TestUpstreamTruncatedBodiesPreserveHTTPStatusAndRecordReadError(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, status string
	}{
		{
			name: "JSON body", contentType: "application/json", status: "completed",
			body: `{"object":"response","status":"completed","model":"synthetic-model"}`,
		},
		{
			name: "unfinished SSE stream", contentType: "text/event-stream", status: "in_progress",
			body: "event: response.in_progress\ndata: {\"response\":{\"object\":\"response\",\"status\":\"in_progress\"}}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				// A short wire body makes the HTTP transport report unexpected EOF.
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+50))
				_, _ = io.WriteString(w, tc.body)
			})
			h := newHarnessWithUpstream(t, "", upstream, nil)
			response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{"model":"request-model"}`, nil)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Errorf("downstream HTTP status = %d, want 200", response.StatusCode)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("downstream body read error = %v, want unexpected EOF", err)
			}
			if string(body) != tc.body {
				t.Errorf("forwarded body differs; received %d bytes, want %d bytes", len(body), len(tc.body))
			}
			// An upstream HTTP response was received, so its stored status stays 200.
			// The read error changes the outcome, even when JSON says "completed".
			got := waitRecords(t, h, 1)
			want := storedRecord{
				user: "alice", model: "request-model", httpStatus: 200,
				status: tc.status, outcome: "incomplete", reason: "response_read_error",
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("accounting records = %+v, want one %+v", got, want)
			}
		})
	}
}
