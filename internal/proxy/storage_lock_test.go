package proxy

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"openai-responses-proxy/internal/config"
)

// Hold SQLite's writer lock until two inserts fail. Both clients must still get
// HTTP 200; after unlocking, only a later successful operation may be stored.
func TestSQLiteBusyWritesAreDiscardedAndLaterCreatesStillRecord(t *testing.T) {
	received := make(chan struct{}, 3)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, func(cfg *config.Config) {
		// Exercise SQLite's five-second busy timeout, rather than a shorter insert deadline.
		cfg.Timeouts.Shutdown = 15 * time.Second
	})
	h.client.Timeout = 20 * time.Second
	// A separate connection holds the write lock without replacing the real store.
	lockDB, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockDB.Close() })
	conn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	})
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	type result struct {
		status int
		err    error
	}
	finished := make(chan result, 2)
	for _, model := range []string{"discarded-a", "discarded-b"} {
		request, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"`+model+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer alice-test-key")
		go func() {
			response, err := h.client.Do(request)
			if err != nil {
				finished <- result{err: err}
				return
			}
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			finished <- result{status: response.StatusCode, err: err}
		}()
	}
	// Both requests must reach upstream while accounting storage is locked.
	for range 2 {
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatal("storage lock prevented request from reaching upstream")
		}
	}
	for range 2 {
		select {
		case got := <-finished:
			if got.err != nil || got.status != http.StatusOK {
				t.Fatalf("create under storage lock: HTTP status = %d, error = %v; want 200, nil", got.status, got.err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("create did not finish after SQLite's busy timeout")
		}
	}
	// Each discarded insert must produce its own error log before we unlock.
	logs, err := os.ReadFile(h.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(logs), "accounting record failed"); got != 2 {
		t.Fatalf("failed-insert log count = %d, want exactly 2", got)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got := len(records(t, h.dbPath)); got != 0 {
		t.Fatalf("rows after releasing lock = %d, want 0", got)
	}

	// A rejected accounting query must not prevent a later create from succeeding.
	badQuery := h.do(t, http.MethodGet, "/accounting?unknown=filter", "alice-test-key", "", nil)
	badQuery.Body.Close()
	if badQuery.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed accounting query status = %d, want 400", badQuery.StatusCode)
	}
	response := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{"model":"recorded-after-unlock"}`, nil)
	_ = responseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Errorf("create after unlocking status = %d, want 200", response.StatusCode)
	}
	// Different model labels make any replay of the two failed inserts visible.
	got := waitRecords(t, h, 1)
	want := storedRecord{
		user: "alice", model: "recorded-after-unlock", httpStatus: 200,
		status: "completed", outcome: "completed", input: 2, output: 3, total: 5,
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("rows after later create = %+v, want only %+v", got, want)
	}
}

// Close the store to make accounting return 503. A subsequent create must still
// reach upstream and return HTTP 200 despite being unable to record its usage.
func TestUnavailableAccountingQueryDoesNotGateUpstreamRequests(t *testing.T) {
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"response","status":"completed"}`)
	}), nil)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	query := h.do(t, http.MethodGet, "/accounting", "alice-test-key", "", nil)
	query.Body.Close()
	if query.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unavailable accounting status = %d, want 503", query.StatusCode)
	}
	create := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	_ = responseBody(t, create)
	if create.StatusCode != http.StatusOK {
		t.Errorf("create after accounting query failure status = %d, want 200", create.StatusCode)
	}
}
