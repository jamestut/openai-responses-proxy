package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openai-responses-proxy/internal/accounting"
	"openai-responses-proxy/internal/auth"
	"openai-responses-proxy/internal/config"
	"openai-responses-proxy/internal/logging"
)

type testHarness struct {
	client   *http.Client
	server   *httptest.Server
	upstream *httptest.Server
	store    *accounting.Store
	logger   *logging.Logger
	dbPath   string
	logPath  string
	cancel   context.CancelFunc
}

func newHarnessWithUpstream(t *testing.T, upstreamURL string, upstreamHandler http.Handler, configure func(*config.Config), levels ...string) *testHarness {
	t.Helper()
	tmp := t.TempDir()
	var upstream *httptest.Server
	var store *accounting.Store
	var logger *logging.Logger
	var proxyServer *Server
	var public *httptest.Server
	var cancel context.CancelFunc
	t.Cleanup(func() {
		if cancel != nil {
			cancel()
		}
		if public != nil {
			public.CloseClientConnections()
			public.Close()
		}
		if proxyServer != nil {
			proxyServer.Close()
		}
		if upstream != nil {
			upstream.CloseClientConnections()
			upstream.Close()
		}
		if store != nil {
			_ = store.Close()
		}
		if logger != nil {
			_ = logger.Close()
		}
	})
	if upstreamHandler != nil {
		upstream = httptest.NewServer(upstreamHandler)
		if upstreamURL == "" {
			upstreamURL = upstream.URL + "/v1"
		}
	}
	if upstream == nil && upstreamURL == "" {
		t.Fatal("upstream URL or handler is required")
	}
	stateDir := filepath.Join(tmp, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	store, err = accounting.Open(filepath.Join(stateDir, "accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "proxy.log")
	level := "upstream_errors"
	if len(levels) > 0 {
		level = levels[0]
	}
	logger, err = logging.New(level, logPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Upstream:            config.Upstream{BaseURL: upstreamURL, APIKey: "upstream-test-secret"},
		Retry:               config.Retry{Delay: 0, MaxRetries: 2},
		Timeouts:            config.Timeouts{Connect: time.Second, TLSHandshake: time.Second, Shutdown: time.Second},
		MaxRequestBodyBytes: 1 << 20,
	}
	if configure != nil {
		configure(&cfg)
	}
	serviceCtx, cancel := context.WithCancel(context.Background())
	users := auth.NewUsers([]config.User{{Name: "alice", APIKeys: []string{"alice-test-key"}}, {Name: "bob", APIKeys: []string{"bob-test-key"}}})
	proxyServer, err = New(cfg, users, store, logger, serviceCtx)
	if err != nil {
		t.Fatal(err)
	}
	public = httptest.NewServer(proxyServer.Handler())
	client := &http.Client{Transport: public.Client().Transport, Timeout: 5 * time.Second}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &testHarness{client: client, server: public, upstream: upstream, store: store, logger: logger, dbPath: filepath.Join(stateDir, "accounting.db"), logPath: logPath, cancel: cancel}
}

func (h *testHarness) do(t *testing.T, method, path, key, body string, headers http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func responseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type storedRecord struct {
	user, model, kind, status, outcome, reason          string
	httpStatus                                          int
	input, output, total, cached, cacheWrite, reasoning int64
}

func records(t *testing.T, dbPath string) []storedRecord {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT username, model, kind, http_status, response_status, outcome, input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens, incomplete_reason FROM accounting_requests ORDER BY started_at_us`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []storedRecord
	for rows.Next() {
		var r storedRecord
		if err := rows.Scan(&r.user, &r.model, &r.kind, &r.httpStatus, &r.status, &r.outcome, &r.input, &r.output, &r.total, &r.cached, &r.cacheWrite, &r.reasoning, &r.reason); err != nil {
			t.Fatal(err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func waitRecords(t *testing.T, h *testHarness, count int) []storedRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result := records(t, h.dbPath)
		if len(result) >= count {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatalf("accounting rows = %d, want at least %d", len(result), count)
		}
		time.Sleep(time.Millisecond)
	}
}

// A create must preserve its path prefix, query, body, and end-to-end headers while replacing credentials.
// Local auth, route, and size rejections must not reach upstream; size rejection records HTTP status 0.
func TestProxyForwardingAuthenticationAndLocalRejection(t *testing.T) {
	var calls atomic.Int32
	var gotMu sync.Mutex
	var gotPath, gotQuery, gotBody, gotAuthorization, gotProxyAuthorization, gotConnectionToken, gotForwarded string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotMu.Lock()
		gotPath, gotQuery, gotAuthorization = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		gotConnectionToken = r.Header.Get("X-Connection-Private")
		gotProxyAuthorization = r.Header.Get("Proxy-Authorization")
		gotForwarded = r.Header.Get("X-Forwarded-Test")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("X-Upstream-Hop", "removed")
		w.Header().Set("X-Upstream-End-To-End", "kept")
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"gpt-6-luna","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, func(cfg *config.Config) {
		cfg.Upstream.BaseURL = strings.Replace(cfg.Upstream.BaseURL, "/v1", "/prefix/v1", 1)
		cfg.MaxRequestBodyBytes = 128
	})
	body := `{"model":"request-model","stream":false}`
	headers := http.Header{"X-Forwarded-Test": []string{"kept"}, "X-Connection-Private": []string{"removed"}, "Connection": []string{"X-Connection-Private"}, "Proxy-Authorization": []string{"Bearer downstream-proxy-secret"}}
	resp := h.do(t, http.MethodPost, "/v1/responses?tag=synthetic%20value", "alice-test-key", body, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	gotMu.Lock()
	requestPath, requestQuery, requestBody := gotPath, gotQuery, gotBody
	requestAuthorization, requestProxyAuthorization := gotAuthorization, gotProxyAuthorization
	requestConnectionToken, requestForwarded := gotConnectionToken, gotForwarded
	gotMu.Unlock()
	if requestPath != "/prefix/v1/responses" || requestQuery != "tag=synthetic%20value" || requestBody != body {
		t.Fatalf("upstream request mismatch: path=%q query=%q body_bytes=%d", requestPath, requestQuery, len(requestBody))
	}
	if requestProxyAuthorization != "" {
		t.Error("Proxy-Authorization reached upstream")
	}
	if resp.Header.Get("Connection") != "" || resp.Header.Get("X-Upstream-Hop") != "" || resp.Header.Get("X-Upstream-End-To-End") != "kept" {
		t.Error("response hop-by-hop filtering removed the wrong headers")
	}
	if requestAuthorization != "Bearer upstream-test-secret" || requestConnectionToken != "" || requestForwarded != "kept" {
		t.Fatalf("upstream credential replacement or hop filtering failed (authorization_replaced=%t, connection_token_removed=%t, end_to_end_header_kept=%t)", requestAuthorization == "Bearer upstream-test-secret", requestConnectionToken == "", requestForwarded == "kept")
	}
	unauthorized := h.do(t, http.MethodPost, "/v1/responses", "wrong-key", `{}`, nil)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	_ = responseBody(t, unauthorized)
	unsupported := h.do(t, http.MethodPost, "/v1/chat/completions", "alice-test-key", `{}`, nil)
	if unsupported.StatusCode != http.StatusNotFound {
		t.Fatalf("unsupported status = %d", unsupported.StatusCode)
	}
	_ = responseBody(t, unsupported)
	tooLarge := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", strings.Repeat("x", 256), nil)
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d", tooLarge.StatusCode)
	}
	_ = responseBody(t, tooLarge)
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	rs := waitRecords(t, h, 2)
	var success, localReject int
	for _, row := range rs {
		if row.httpStatus == http.StatusOK && row.model == "request-model" && row.input == 2 && row.user == "alice" {
			success++
		}
		if row.httpStatus == 0 && row.reason == "" {
			localReject++
		}
	}
	if len(rs) != 2 || success != 1 || localReject != 1 {
		t.Fatalf("accounting metadata mismatch (rows=%d success=%d local_rejection=%d)", len(rs), success, localReject)
	}
}

// Utility and retrieval routes must forward the requested method and path.
// Even when their responses contain usage, they must not create accounting rows.
func TestUtilityAndRetrievalRoutesForwardWithoutAccounting(t *testing.T) {
	type received struct{ method, path string }
	var mu sync.Mutex
	var got []received
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, received{r.Method, r.URL.Path})
		mu.Unlock()
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":9,"total_tokens":9}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/responses/input_tokens"},
		{http.MethodGet, "/v1/responses/resp_synthetic"},
		{http.MethodDelete, "/v1/responses/resp_synthetic"},
		{http.MethodPost, "/v1/responses/resp_synthetic/cancel"},
		{http.MethodGet, "/v1/responses/resp_synthetic/input_items"},
	}
	for _, tc := range cases {
		resp := h.do(t, tc.method, tc.path, "alice-test-key", `{}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s status = %d", tc.method, tc.path, resp.StatusCode)
		}
		_ = responseBody(t, resp)
	}
	if len(waitRecords(t, h, 0)) != 0 {
		t.Fatalf("utility routes created accounting rows: %+v", records(t, h.dbPath))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(cases) {
		t.Fatalf("upstream calls = %+v", got)
	}
	for i, tc := range cases {
		if got[i] != (received{tc.method, tc.path}) {
			t.Fatalf("upstream call %d = %+v, want %+v", i, got[i], tc)
		}
	}
}

// Each JSON create or compact must record its own usage and diagnostic metadata once.
// The cases distinguish missing usage, nonterminal responses, invalid JSON, and upstream errors.
func TestJSONCreateCompactAndInvalidResponseOutcomes(t *testing.T) {
	cases := []struct {
		name, path, body, kind, model   string
		httpStatus                      int
		responseStatus, outcome, reason string
		input, total                    int64
	}{
		{"completed create", "/v1/responses", `{"object":"response","status":"completed","model":"returned-model","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, "create", "request-model", http.StatusOK, "completed", "completed", "", 2, 5},
		{"completed compact", "/v1/responses/compact", `{"object":"response.compaction","status":"completed","model":"compact-model","usage":{"input_tokens":4,"total_tokens":4}}`, "compact", "request-model", http.StatusOK, "completed", "completed", "", 4, 4},
		{"compact object without status", "/v1/responses/compact", `{"object":"response.compaction","model":"compact-model","usage":{"input_tokens":4,"total_tokens":4}}`, "compact", "request-model", http.StatusOK, "", "completed", "", 4, 4},
		{"completed response without usage", "/v1/responses", `{"object":"response","status":"completed","model":"m"}`, "create", "request-model", http.StatusOK, "completed", "completed", "", 0, 0},
		{"nonterminal JSON with usage", "/v1/responses", `{"object":"response","status":"queued","model":"m","usage":{"input_tokens":6,"total_tokens":6}}`, "create", "request-model", http.StatusOK, "queued", "incomplete", "nonterminal_response", 6, 6},
		{"unrelated JSON", "/v1/responses", `{"ok":true}`, "create", "request-model", http.StatusOK, "", "incomplete", "invalid_response", 0, 0},
		{"malformed JSON", "/v1/responses", `{`, "create", "request-model", http.StatusOK, "", "incomplete", "invalid_response", 0, 0},
		{"HTTP error with usage", "/v1/responses", `{"object":"response","status":"in_progress","usage":{"input_tokens":99,"total_tokens":99}}`, "create", "request-model", http.StatusBadGateway, "in_progress", "failed", "", 99, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.httpStatus >= 400 {
					w.WriteHeader(tc.httpStatus)
				}
				_, _ = io.WriteString(w, tc.body)
			})
			h := newHarnessWithUpstream(t, "", upstream, nil)
			resp := h.do(t, http.MethodPost, tc.path, "alice-test-key", `{"model":"request-model"}`, nil)
			if resp.StatusCode != tc.httpStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.httpStatus)
			}
			_ = responseBody(t, resp)
			rs := waitRecords(t, h, 1)
			if len(rs) != 1 || rs[0].model != tc.model || rs[0].kind != tc.kind || rs[0].httpStatus != tc.httpStatus || rs[0].status != tc.responseStatus || rs[0].outcome != tc.outcome || rs[0].reason != tc.reason || rs[0].input != tc.input || rs[0].total != tc.total {
				t.Fatalf("record = %+v", rs)
			}
		})
	}
}

// Two 429s followed by success must replay the same request and produce one accounting row.
// Retry-After must not override the configured delay, and generated delays must stay within jitter bounds.
func TestRetryPolicyReplaysRequestsAndRecordsOnce(t *testing.T) {
	var calls atomic.Int32
	var requestMu sync.Mutex
	var bodies []string
	var paths []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestMu.Lock()
		bodies = append(bodies, string(body))
		paths = append(paths, r.URL.RequestURI())
		requestMu.Unlock()
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "99")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"synthetic retry"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","model":"m","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, func(cfg *config.Config) { cfg.Retry.Delay = 20 * time.Millisecond; cfg.Retry.MaxRetries = 2 })
	started := time.Now()
	resp := h.do(t, http.MethodPost, "/v1/responses?repeat=1", "alice-test-key", `{"model":"m"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	elapsed := time.Since(started)
	if calls.Load() != 3 || elapsed < 25*time.Millisecond {
		t.Fatalf("calls = %d, elapsed = %s", calls.Load(), elapsed)
	}
	requestMu.Lock()
	gotBodies, gotPaths := append([]string(nil), bodies...), append([]string(nil), paths...)
	requestMu.Unlock()
	for i := range gotBodies {
		if gotBodies[i] != `{"model":"m"}` || gotPaths[i] != "/v1/responses?repeat=1" {
			t.Fatalf("attempt %d replay mismatch: path=%q body_bytes=%d", i, gotPaths[i], len(gotBodies[i]))
		}
	}
	rs := waitRecords(t, h, 1)
	if len(rs) != 1 || rs[0].input != 1 || rs[0].total != 3 {
		t.Fatalf("records = %+v", rs)
	}
	for i := 0; i < 1000; i++ {
		d := retryDelay(time.Second)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("retry delay out of bounds: %s", d)
		}
	}
}

// Exhausted retries must return the last 429 body. Other statuses must return after one attempt,
// including redirects, with one accounting row describing each final upstream response.
func TestRetryExhaustionOtherStatusesAndRedirect(t *testing.T) {
	statuses := []int{429, 429, 429, 400, 500, 302}
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		if i >= len(statuses) {
			t.Error("proxy made an unexpected additional upstream attempt")
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if statuses[i] == http.StatusFound {
			w.Header().Set("Location", "/v1/responses")
		}
		w.WriteHeader(statuses[i])
		_, _ = fmt.Fprintf(w, `{"error":{"message":"status-%d-attempt-%d"}}`, statuses[i], i+1)
	})
	h := newHarnessWithUpstream(t, "", upstream, func(cfg *config.Config) { cfg.Retry.MaxRetries = 2 })
	for i, want := range []int{429, 400, 500, 302} {
		resp := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
		data := responseBody(t, resp)
		wantBody := []string{
			`{"error":{"message":"status-429-attempt-3"}}`,
			`{"error":{"message":"status-400-attempt-4"}}`,
			`{"error":{"message":"status-500-attempt-5"}}`,
			`{"error":{"message":"status-302-attempt-6"}}`,
		}
		if resp.StatusCode != want || data != wantBody[i] {
			t.Fatalf("case %d upstream response mismatch: status=%d body_bytes=%d, want status=%d", i, resp.StatusCode, len(data), want)
		}
	}
	if calls.Load() != 6 { // three 429 attempts plus one call for each of 400, 500, and 302 requests
		t.Fatalf("upstream calls = %d, want 6", calls.Load())
	}
	rs := waitRecords(t, h, 4)
	if len(rs) != 4 {
		t.Fatalf("accounting records = %d, want 4", len(rs))
	}
	wantRecords := map[int]storedRecord{
		429: {user: "alice", kind: "create", httpStatus: 429, status: "failed", outcome: "failed"},
		400: {user: "alice", kind: "create", httpStatus: 400, status: "failed", outcome: "failed"},
		500: {user: "alice", kind: "create", httpStatus: 500, status: "failed", outcome: "failed"},
		302: {user: "alice", kind: "create", httpStatus: 302, outcome: "incomplete", reason: "invalid_response"},
	}
	for _, row := range rs {
		want, ok := wantRecords[row.httpStatus]
		if !ok || row != want {
			t.Fatalf("stored HTTP outcome = %+v; want %+v (known status=%v)", row, want, ok)
		}
		delete(wantRecords, row.httpStatus)
	}
}

// A retrieval must retry a 429 and forward the successful response.
// Those attempts must leave accounting empty, even if the response includes usage.
func TestRetrievalRetries429WithoutAccounting(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":99,"total_tokens":99}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	resp := h.do(t, http.MethodGet, "/v1/responses/resp_synthetic?include=usage", "alice-test-key", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	if calls.Load() != 2 {
		t.Fatalf("GET attempts = %d, want 2", calls.Load())
	}
	if rows := records(t, h.dbPath); len(rows) != 0 {
		t.Fatalf("retrieval inserted accounting rows: %+v", rows)
	}
}

// Hold all three creates upstream to prove they overlap and have no accounting rows yet.
// Releasing them must yield three successful responses and three attributed records.
func TestConcurrentCreatesOverlapAndEachRecordOnce(t *testing.T) {
	const count = 3
	arrivals := make(chan struct{}, count)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		arrivals <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","model":"m","usage":{"input_tokens":1,"total_tokens":1}}`)
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	responses := make(chan *http.Response, count)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer alice-test-key")
			resp, err := h.client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			responses <- resp
		}()
	}
	for i := 0; i < count; i++ {
		// Each upstream arrival signals that this create is in flight; release only after all three arrive to prove overlap.
		select {
		case <-arrivals:
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("creates did not reach upstream concurrently")
		}
	}
	if rows := records(t, h.dbPath); len(rows) != 0 {
		t.Fatalf("operations were accounted before upstream completion: %d rows", len(rows))
	}
	unblock()
	for i := 0; i < count; i++ {
		select {
		case resp := <-responses:
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			_ = responseBody(t, resp)
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("create did not finish")
		}
	}
	if calls.Load() != count {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	rs := waitRecords(t, h, count)
	if len(rs) != count {
		t.Fatalf("records = %d", len(rs))
	}
}

// Cancel during a retry wait and during active upstream work. Both requests must stop
// and record client_disconnected; cancellation during the wait must retain the prior HTTP 429.
func TestCancelDuringRetryWaitAndActiveUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		<-r.Context().Done()
	})
	h := newHarnessWithUpstream(t, "", upstream, func(cfg *config.Config) { cfg.Retry.Delay = time.Second; cfg.Retry.MaxRetries = 5 })
	client := &http.Client{Transport: h.server.Client().Transport}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer alice-test-key")
	done := make(chan struct{})
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		_ = err
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		logData, _ := os.ReadFile(h.logPath)
		if strings.Contains(string(logData), "retrying") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry wait did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not stop after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("retry calls = %d", calls.Load())
	}
	rs := waitRecords(t, h, 1)
	if len(rs) != 1 || rs[0].httpStatus != 429 || rs[0].reason != "client_disconnected" {
		t.Fatalf("retry cancellation record = %+v", rs)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	activeUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	h2 := newHarnessWithUpstream(t, "", activeUpstream, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodPost, h2.server.URL+"/v1/responses", strings.NewReader(`{"model":"m"}`))
	req2.Header.Set("Authorization", "Bearer alice-test-key")
	done2 := make(chan struct{})
	go func() {
		resp, err := h2.client.Do(req2)
		if resp != nil {
			_ = resp.Body.Close()
		}
		_ = err
		close(done2)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel2()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("active request did not stop after cancellation")
	}
	rs = waitRecords(t, h2, 1)
	if len(rs) != 1 || rs[0].reason != "client_disconnected" || rs[0].httpStatus != 0 {
		t.Fatalf("active cancellation record = %+v", rs)
	}
}

// Raw SSE bytes must reach the client unchanged. Only a complete terminal event may supply usage;
// provisional or unterminated data must not count, and later events must not replace terminal metadata.
func TestSSEFramingUsageAndTerminalMetadata(t *testing.T) {
	cases := []struct {
		name, stream, wantReason, wantStatus, wantOutcome               string
		wantInput, wantCached, wantCacheWrite, wantReasoning, wantTotal int64
	}{
		{
			name: "complete terminal usage includes standard details",
			stream: `event: response.completed
data: {"response":{"object":"response","status":"completed","model":"m","usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":16}}}

`,
			wantStatus: "completed", wantOutcome: "completed", wantInput: 12, wantCached: 2, wantCacheWrite: 1, wantReasoning: 1, wantTotal: 16,
		},
		{
			name: "provisional usage is discarded when terminal usage is missing",
			stream: `event: response.in_progress
data: {"response":{"object":"response","status":"in_progress","model":"m","usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}

event: response.completed
data: {"response":{"object":"response","status":"completed","model":"m"}}

`,
			wantStatus: "completed", wantOutcome: "completed",
		},
		{
			name: "terminal frame without blank line is forwarded but ignored",
			stream: `event: response.completed
data: {"response":{"object":"response","status":"completed","model":"m","usage":{"input_tokens":8,"total_tokens":8}}}
`,
			wantReason: "stream_ended", wantOutcome: "incomplete",
		},
		{
			name: "whitespace line is not a delimiter",
			stream: `event: response.completed
data: {"response":{"object":"response","status":"completed","model":"m","usage":{"input_tokens":8,"total_tokens":8}}}
 
`,
			wantReason: "stream_ended", wantOutcome: "incomplete",
		},
		{
			name:       "failed terminal supports CRLF and multiline data",
			stream:     ": heartbeat\r\nevent: response.failed\r\ndata: {\"response\":{\r\ndata: \"object\":\"response\",\"status\":\"failed\",\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"total_tokens\":3}}}\r\n\r\n",
			wantStatus: "failed", wantOutcome: "failed", wantInput: 3, wantTotal: 3,
		},
		{
			name: "incomplete terminal records terminal usage",
			stream: `event: response.incomplete
data: {"response":{"object":"response","status":"incomplete","model":"m","usage":{"input_tokens":5,"total_tokens":5}}}

`,
			wantStatus: "incomplete", wantOutcome: "incomplete", wantInput: 5, wantTotal: 5,
		},
		{
			name: "later nonterminal event cannot replace terminal usage",
			stream: `event: response.completed
data: {"response":{"object":"response","status":"completed","model":"m","usage":{"input_tokens":7,"total_tokens":7}}}

event: response.in_progress
data: {"response":{"object":"response","status":"in_progress","model":"other","usage":{"input_tokens":99,"total_tokens":99}}}

`,
			wantStatus: "completed", wantOutcome: "completed", wantInput: 7, wantTotal: 7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			})
			h := newHarnessWithUpstream(t, "", upstream, nil)
			resp := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{"model":"request-model","stream":true}`, nil)
			got := responseBody(t, resp)
			if got != tc.stream {
				t.Fatalf("stream bytes changed: got %d bytes, want %d bytes", len(got), len(tc.stream))
			}
			rs := waitRecords(t, h, 1)
			if len(rs) != 1 || rs[0].input != tc.wantInput || rs[0].reason != tc.wantReason || rs[0].status != tc.wantStatus || rs[0].outcome != tc.wantOutcome || rs[0].cached != tc.wantCached || rs[0].cacheWrite != tc.wantCacheWrite || rs[0].reasoning != tc.wantReasoning || rs[0].total != tc.wantTotal {
				t.Fatalf("record = %+v", rs)
			}
		})
	}
}

// Hold the terminal event until the client receives the first output byte.
// Receiving that byte before release proves the proxy does not buffer the whole stream.
func TestSSEFlushesBeforeTerminalEvent(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"delta\":\"x\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"total_tokens\":1}}}\n\n")
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer alice-test-key")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "e" {
		t.Fatalf("first byte = %q", buf)
	}
	unblock()
	remaining, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(string(remaining), "response.completed") {
		t.Fatalf("terminal frame missing from %d remaining bytes", len(remaining))
	}
}

// Keep upstream open after a heartbeat. The client must receive its comment and delimiter
// before upstream is released, even though there is no response data event.
func TestSSEHeartbeatFlushesPromptly(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": heartbeat\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	h := newHarnessWithUpstream(t, "", upstream, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+"/v1/responses/resp_synthetic", nil)
	req.Header.Set("Authorization", "Bearer alice-test-key")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != ": heartbeat\n" {
		t.Fatalf("heartbeat line = %q, err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "\n" {
		t.Fatalf("heartbeat delimiter = %q, err=%v", line, err)
	}
	unblock()
}

// The custom CA must enable a localhost certificate without disabling hostname checks.
// Untrusted and wrong-hostname connections must fail before the upstream HTTP handler runs.
func TestTLSVerificationAndCustomCA(t *testing.T) {
	cert, key, caPEM := testCertificate(t)
	certFile := filepath.Join(t.TempDir(), "server.pem")
	keyFile := filepath.Join(t.TempDir(), "server-key.pem")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(certFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":1,"total_tokens":1}}`)
	}))
	upstream.Listener = listener
	upstream.TLS = nil
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{*tlsCertificate(certFile, keyFile)}}
	upstream.StartTLS()
	defer upstream.Close()
	base := "https://localhost:" + strings.Split(listener.Addr().String(), ":")[1] + "/v1"
	h := newHarnessWithUpstream(t, base, nil, nil)
	resp := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("untrusted status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	if calls.Load() != 0 {
		t.Fatalf("untrusted handshake reached HTTP handler %d times", calls.Load())
	}
	if rs := waitRecords(t, h, 1); len(rs) != 1 || rs[0].httpStatus != 0 || rs[0].reason != "transport_error" {
		t.Fatalf("untrusted record = %+v", rs)
	}
	// The certificate SAN is localhost: a configured CA enables that hostname, while dialing its IP must still fail hostname verification.
	h2 := newHarnessWithUpstream(t, base, nil, func(cfg *config.Config) { cfg.Upstream.CAFile = caFile })
	resp = h2.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	if resp.StatusCode != http.StatusOK {
		_ = responseBody(t, resp)
		t.Fatalf("trusted upstream status = %d, want 200", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	if calls.Load() != 1 {
		t.Fatalf("trusted calls = %d", calls.Load())
	}
	h3 := newHarnessWithUpstream(t, "https://127.0.0.1:"+strings.Split(listener.Addr().String(), ":")[1]+"/v1", nil, func(cfg *config.Config) { cfg.Upstream.CAFile = caFile })
	resp = h3.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("hostname mismatch status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	if calls.Load() != 1 {
		t.Fatalf("hostname mismatch reached HTTP handler; calls=%d", calls.Load())
	}
}

// Queued JSON, invalid JSON, and unterminated SSE must each leave one incomplete, zero-usage row.
// Restart and later client retrievals must preserve that row without automatic retrieval or recovery.
func TestBackgroundRetrievalDoesNotRecoverAccounting(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body, reason string }{
		{"queued JSON", "application/json", `{"object":"response","status":"queued","model":"m","id":"resp_synthetic"}`, "nonterminal_response"},
		{"unrelated JSON", "application/json", `{"ok":true}`, "invalid_response"},
		{"unterminated terminal SSE", "text/event-stream", "event: response.completed\ndata: {\"response\":{\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"total_tokens\":99}}}\n", "stream_ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets atomic.Int32
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
					if r.URL.Path == "/v1/responses/absent" {
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"error":{"message":"synthetic missing response"}}`)
						return
					}
					_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":99,"total_tokens":99}}`)
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			})
			h := newHarnessWithUpstream(t, "", upstream, nil)
			resp := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{"model":"m","background":true}`, nil)
			_ = responseBody(t, resp)
			rs := waitRecords(t, h, 1)
			want := storedRecord{user: "alice", model: "m", kind: "create", httpStatus: 200, outcome: "incomplete", reason: tc.reason}
			if tc.reason == "nonterminal_response" {
				want.status = "queued"
			}
			if len(rs) != 1 || rs[0] != want {
				t.Fatalf("background record = %+v, want exactly %+v", rs, want)
			}
			if gets.Load() != 0 {
				t.Fatalf("automatic GET count = %d", gets.Load())
			}
			// Reopen the same database under a fresh proxy to check that restart does not schedule retrieval or revise the original row.
			h.server.Close()
			h.cancel()
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := accounting.Open(h.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			h.store = reopened
			ctx, cancel := context.WithCancel(context.Background())
			cfg := config.Config{Upstream: config.Upstream{BaseURL: h.upstream.URL + "/v1", APIKey: "upstream-test-secret"}, Retry: config.Retry{MaxRetries: 2}, Timeouts: config.Timeouts{Shutdown: time.Second}, MaxRequestBodyBytes: 1 << 20}
			proxyServer, err := New(cfg, auth.NewUsers([]config.User{{Name: "alice", APIKeys: []string{"alice-test-key"}}, {Name: "bob", APIKeys: []string{"bob-test-key"}}}), reopened, h.logger, ctx)
			if err != nil {
				t.Fatal(err)
			}
			public := httptest.NewServer(proxyServer.Handler())
			h.server = public
			h.client = &http.Client{Transport: public.Client().Transport, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			t.Cleanup(func() {
				cancel()
				public.CloseClientConnections()
				public.Close()
				proxyServer.Close()
				_ = reopened.Close()
			})
			if gets.Load() != 0 {
				t.Error("proxy restart triggered automatic upstream retrieval")
			}
			beforeRetrieval := rs[0]
			if rows := records(t, h.dbPath); len(rows) != 1 || rows[0] != beforeRetrieval {
				t.Fatalf("persisted record changed on reopen: %+v", rows)
			}
			for _, key := range []string{"alice-test-key", "bob-test-key"} {
				resp = h.do(t, http.MethodGet, "/v1/responses/resp_synthetic", key, "", nil)
				_ = responseBody(t, resp)
			}
			missing := h.do(t, http.MethodGet, "/v1/responses/absent", "alice-test-key", "", nil)
			if missing.StatusCode != http.StatusNotFound {
				t.Fatalf("missing retrieval status = %d", missing.StatusCode)
			}
			_ = responseBody(t, missing)
			rs = waitRecords(t, h, 1)
			if gets.Load() != 3 || len(rs) != 1 || rs[0] != beforeRetrieval {
				t.Fatalf("gets=%d records=%+v", gets.Load(), rs)
			}
		})
	}
}

// Reject an insert at each log level: forwarding must still succeed and log the failure.
// After removing the rejection, a new create must record normally without replaying the lost insert.
func TestStorageFailureDoesNotBlockForwardingAndLogsAtEveryLevel(t *testing.T) {
	for _, level := range []string{"upstream_errors", "error", "warn", "info", "debug"} {
		t.Run(level, func(t *testing.T) {
			var calls atomic.Int32
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{"object":"response","status":"completed","model":"m","usage":{"input_tokens":1,"total_tokens":1}}`)
			})
			h := newHarnessWithUpstream(t, "", upstream, nil, level)
			db, err := sql.Open("sqlite", h.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`CREATE TRIGGER reject_accounting BEFORE INSERT ON accounting_requests BEGIN SELECT RAISE(ABORT, 'synthetic storage failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
			resp := h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			_ = responseBody(t, resp)
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d", calls.Load())
			}
			if rows := records(t, h.dbPath); len(rows) != 0 {
				t.Fatalf("failed insert unexpectedly persisted: %+v", rows)
			}
			_, err = db.Exec(`DROP TRIGGER reject_accounting`)
			if err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			resp = h.do(t, http.MethodPost, "/v1/responses", "alice-test-key", `{}`, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("second status = %d", resp.StatusCode)
			}
			_ = responseBody(t, resp)
			rows := waitRecords(t, h, 1)
			if calls.Load() != 2 || len(rows) != 1 || rows[0].input != 1 {
				t.Fatalf("calls=%d rows=%+v", calls.Load(), rows)
			}
			logData, err := os.ReadFile(h.logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(logData), "accounting record failed") {
				t.Fatalf("missing accounting failure log: %s", logData)
			}
		})
	}
}

// An unused loopback port must produce a transport-error row with HTTP status 0.
// Its log must omit the query marker and endpoint address.
func TestTransportErrorLogDoesNotExposeURLQuery(t *testing.T) {
	// An unused loopback port produces a local connection failure without a live dependency.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	h := newHarnessWithUpstream(t, "http://"+addr+"/v1", nil, nil)
	resp := h.do(t, http.MethodPost, "/v1/responses?secret-query-marker", "alice-test-key", `{}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = responseBody(t, resp)
	logData, _ := os.ReadFile(h.logPath)
	if strings.Contains(string(logData), "secret-query-marker") || strings.Contains(string(logData), addr) {
		t.Fatal("transport log contains query or endpoint details")
	}
	rs := waitRecords(t, h, 1)
	if len(rs) != 1 || rs[0].httpStatus != 0 || rs[0].reason != "transport_error" {
		t.Fatalf("transport record = %+v", rs)
	}
}

func testCertificate(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "proxy-test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return cert, key, ca
}

func tlsCertificate(certFile, keyFile string) *tls.Certificate {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		panic(err)
	}
	return &certificate
}
