package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"openai-responses-proxy/internal/accounting"
	"openai-responses-proxy/internal/testutil"
)

const helperConfigEnv = "RESPONSES_PROXY_TEST_CONFIG"
const helperCommandEnv = "RESPONSES_PROXY_TEST_COMMAND"

// TestCommandHelper runs a real command in a subprocess so serve can receive OS signals.
// It stays inert during the ordinary test run unless both helper settings are present.
func TestCommandHelper(t *testing.T) {
	configPath, commandName := os.Getenv(helperConfigEnv), os.Getenv(helperCommandEnv)
	if configPath == "" || commandName == "" {
		return
	}
	os.Exit(run([]string{commandName, configPath}))
}

// Invalid command names and argument counts return the usage-error exit code.
func TestCommandArgumentValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"version", "extra"}, {"serve"}, {"reload-keys", ""}} {
		if got := run(args); got != 2 {
			t.Errorf("run(%q) = %d, want 2", args, got)
		}
	}
}

// Invalid configuration must fail before creating state. Invalid users must fail
// before opening the database or reload socket.
func TestServeRejectsInvalidConfigurationAndUsersBeforeStarting(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	configPath, stateDir := writeConfig(t, upstream.URL, "1s", "alice", "alice-key")
	listen := readListen(t, configPath)
	if err := os.WriteFile(configPath, []byte("not: [valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := runHelper(t, "serve", configPath); status != 1 {
		t.Fatalf("invalid main config exit = %d, want 1", status)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("invalid config created state directory: %v", err)
	}

	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("listen: %q\nupstream:\n  base_url: %q\n  api_key: mock-key\nusers_file: users.yaml\nstate_dir: state\n", listen, strings.TrimRight(upstream.URL, "/")+"/v1/responses")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "users.yaml"), []byte("users:\n  - name: alice\n    api_keys: [duplicate]\n  - name: bob\n    api_keys: [duplicate]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := runHelper(t, "serve", configPath); status != 1 {
		t.Fatalf("invalid users exit = %d, want 1", status)
	}
	for _, path := range []string{filepath.Join(stateDir, "reload.sock"), filepath.Join(stateDir, "accounting.sqlite")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid users startup created %s: %v", path, err)
		}
	}
}

// A valid reload replaces keys, a failed reload preserves them, and a request already authenticated as Bob stays attributed to Bob while shutdown drains it.
func TestServeReloadPinsInFlightUsernameAndDrainsOnSignal(t *testing.T) {
	reached, release := make(chan struct{}, 1), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"gpt-6-luna","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	configPath, stateDir := writeConfig(t, upstream.URL, "2s", "alice", "alice-key", "bob", "bob-key")
	process := startHelper(t, "serve", configPath)
	defer stopHelper(process)
	waitProxyReady(t, process.listen)
	assertMode(t, stateDir, 0700)
	assertMode(t, filepath.Join(stateDir, "reload.sock"), 0600)

	requestDone := make(chan clientResult, 1)
	// Hold Bob's upstream request until after both reload attempts to check its original identity.
	go func() { requestDone <- create(process.listen, "bob-key") }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("mock upstream did not receive the request")
	}
	writeUsers(t, configPath, "alice", "alice-key")
	if status := runHelper(t, "reload-keys", configPath); status != 0 {
		t.Fatalf("reload-keys exited with %d", status)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "users.yaml"), []byte("users:\n  - name: alice\n    api_keys: [conflict]\n  - name: bob\n    api_keys: [conflict]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := runHelper(t, "reload-keys", configPath); status == 0 {
		t.Fatal("reload-keys succeeded with duplicate users-file keys")
	}
	// Alice's current key remains usable after validation fails; Bob's removed key is no longer accepted.
	if got := queryAccounting(process.listen, "alice-key"); got != http.StatusOK {
		t.Fatalf("prior valid Alice key after failed reload status = %d, want 200", got)
	}
	if got := create(process.listen, "bob-key").status; got != http.StatusUnauthorized {
		t.Fatalf("removed key status = %d, want 401", got)
	}
	writeUsers(t, configPath, "alice", "alice-key")
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitListenerClosed(t, process.listen)
	releaseUpstream()
	select {
	case result := <-requestDone:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("in-flight request status=%d err=%v", result.status, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish")
	}

	if err := waitHelper(t, process, 4*time.Second); err != nil {
		t.Fatalf("serve shutdown: %v; output: %s", err, process.output())
	}
	logs := process.output()
	if !strings.Contains(logs, "downstream authentication failed") || strings.Contains(logs, "bob-key") {
		t.Fatal("authentication log was missing its event or contained an Authorization value")
	}
	assertRequests(t, stateDir, "bob", 1)
	if _, err := os.Stat(filepath.Join(stateDir, "reload.sock")); !os.IsNotExist(err) {
		t.Fatalf("reload socket remains after shutdown: %v", err)
	}
}

// A blocked insert must not keep the service alive indefinitely during shutdown.
// Once discarded, its row must remain absent after restart.
func TestServeDeadlineDropsLockedInsertAcrossRestart(t *testing.T) {
	reached, sent := make(chan struct{}, 1), make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_2","model":"gpt-6-luna","status":"completed","usage":{"input_tokens":7,"output_tokens":8,"total_tokens":15}}`)
		sent <- struct{}{}
	}))
	defer upstream.Close()
	configPath, stateDir := writeConfig(t, upstream.URL, "350ms", "alice", "alice-key")
	process := startHelper(t, "serve", configPath)
	waitProxyReady(t, process.listen)
	lockDB, err := sql.Open("sqlite", sqliteFile(filepath.Join(stateDir, "accounting.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	lockDB.SetMaxOpenConns(1)
	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		_ = lockConn.Close()
		_ = lockDB.Close()
	}()
	requestDone := make(chan clientResult, 1)
	go func() { requestDone <- create(process.listen, "alice-key") }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("mock upstream did not receive the locked-write request")
	}
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("mock upstream did not finish its response")
	}
	// The response has arrived, but the handler remains active while its locked write is unresolved.
	select {
	case result := <-requestDone:
		t.Fatalf("request completed while the accounting database remained locked: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	start := time.Now()
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitHelper(t, process, 3*time.Second); err == nil {
		t.Fatalf("deadline shutdown unexpectedly succeeded; output: %s", process.output())
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("deadline shutdown exit = %v; output: %s", err, process.output())
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("request did not stop after the shutdown deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown exceeded bound: %s", elapsed)
	}
	_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
	_ = lockConn.Close()
	_ = lockDB.Close()
	assertRequests(t, stateDir, "", 0)

	process = startHelper(t, "serve", configPath)
	waitProxyReady(t, process.listen)
	assertRequests(t, stateDir, "", 0)
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := waitHelper(t, process, 3*time.Second); err != nil {
		t.Fatalf("restart shutdown: %v", err)
	}
}

// Deadline expiry cancels active upstream work; restart must not replay or retrieve that operation.
func TestServeDeadlineCancelsActiveUpstreamAndDoesNotReplayOrRetrieve(t *testing.T) {
	reached := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	requests := make(chan string, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- r.Method + " " + r.URL.Path
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
		select {
		case canceled <- struct{}{}:
		default:
		}
	}))
	defer upstream.Close()
	defer releaseOnce.Do(func() { close(release) })
	configPath, stateDir := writeConfig(t, upstream.URL, "350ms", "alice", "alice-key")
	process := startHelper(t, "serve", configPath)
	waitProxyReady(t, process.listen)
	requestDone := make(chan clientResult, 1)
	go func() { requestDone <- createWithTimeout(process.listen, "alice-key", 10*time.Second) }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("mock upstream did not receive the active request")
	}
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy shutdown did not cancel the active upstream request")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream request did not stop after shutdown")
	}
	if err := waitHelper(t, process, 3*time.Second); err == nil {
		t.Fatalf("shutdown past its deadline unexpectedly succeeded; output: %s", process.output())
	}
	first, ok := <-requests
	if !ok || first != "POST /v1/responses" {
		t.Fatalf("upstream request = %q, want only create", first)
	}
	beforeRestart := optionalCanceledRecord(t, stateDir)

	process = startHelper(t, "serve", configPath)
	waitProxyReady(t, process.listen)
	// Check a bounded startup window for automatic replay or retrieval traffic.
	select {
	case extra := <-requests:
		t.Fatalf("restart issued unexpected upstream request %q", extra)
	case <-time.After(150 * time.Millisecond):
	}
	afterRestart := optionalCanceledRecord(t, stateDir)
	if beforeRestart != afterRestart {
		t.Fatalf("restart changed the canceled operation record: before=%+v after=%+v", beforeRestart, afterRestart)
	}
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := waitHelper(t, process, 3*time.Second); err != nil {
		t.Fatalf("restart shutdown: %v", err)
	}
}

type canceledRecord struct {
	count                                               int
	username, model                                     string
	httpStatus                                          int
	responseStatus, outcome, incompleteReason           string
	input, output, total, cached, cacheWrite, reasoning int64
}

func optionalCanceledRecord(t *testing.T, stateDir string) canceledRecord {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFile(filepath.Join(stateDir, "accounting.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT username, model, http_status, response_status, outcome,
incomplete_reason, input_tokens, output_tokens, total_tokens, cached_tokens,
cache_write_tokens, reasoning_tokens FROM accounting_requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var record canceledRecord
	for rows.Next() {
		record.count++
		if record.count > 1 {
			t.Fatal("deadline-canceled operation produced multiple rows")
		}
		if err := rows.Scan(&record.username, &record.model, &record.httpStatus,
			&record.responseStatus, &record.outcome, &record.incompleteReason, &record.input,
			&record.output, &record.total, &record.cached, &record.cacheWrite, &record.reasoning); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if record.count == 0 {
		// Best-effort accounting may lose this operation when the shutdown deadline expires.
		return record
	}
	if record.username != "alice" || record.model != "gpt-6-luna" ||
		record.outcome != "incomplete" || record.incompleteReason != "client_disconnected" || record.httpStatus != 0 ||
		record.responseStatus != "" || record.input != 0 || record.output != 0 || record.total != 0 ||
		record.cached != 0 || record.cacheWrite != 0 || record.reasoning != 0 {
		t.Fatalf("unexpected deadline-canceled record: %+v", record)
	}
	return record
}

type clientResult struct {
	status int
	err    error
}
type helperProcess struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	listen         string
}

func (p *helperProcess) output() string { return p.stdout.String() + p.stderr.String() }

func startHelper(t *testing.T, commandName, configPath string) *helperProcess {
	t.Helper()
	// Re-enter only TestCommandHelper with explicit environment settings; never execute the service test suite recursively.
	p := &helperProcess{cmd: exec.Command(os.Args[0], "-test.run=^TestCommandHelper$"), listen: readListen(t, configPath)}
	p.cmd.Env = append(os.Environ(), helperConfigEnv+"="+configPath, helperCommandEnv+"="+commandName)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
	return p
}

func runHelper(t *testing.T, commandName, configPath string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandHelper$")
	cmd.Env = append(os.Environ(), helperConfigEnv+"="+configPath, helperCommandEnv+"="+commandName)
	if err := cmd.Run(); err == nil {
		return 0
	} else if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	} else {
		t.Fatal(err)
	}
	return -1
}

func waitHelper(t *testing.T, p *helperProcess, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		_ = p.cmd.Process.Kill()
		err := <-done
		return fmt.Errorf("process did not exit in %s (reaped with %v)", limit, err)
	}
}

func stopHelper(p *helperProcess) {
	if p.cmd.ProcessState == nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
}

func writeConfig(t *testing.T, upstream, shutdown string, users ...string) (string, string) {
	t.Helper()
	root := testutil.ShortTempDir(t)
	configPath, stateDir := filepath.Join(root, "proxy.yaml"), filepath.Join(root, "state")
	listen := reserveAddress(t)
	contents := fmt.Sprintf("listen: %q\nupstream:\n  base_url: %q\n  api_key: mock-key\nusers_file: users.yaml\nstate_dir: state\nretry:\n  delay: 1ms\n  max_retries: 0\ntimeouts:\n  connect: 1s\n  tls_handshake: 1s\n  response_header: 0s\n  total_request: 0s\n  shutdown: %s\nlogging:\n  level: debug\n  destination: stdout\n", listen, strings.TrimRight(upstream, "/")+"/v1/responses", shutdown)
	if err := os.WriteFile(configPath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	writeUsers(t, configPath, users...)
	return configPath, stateDir
}

func writeUsers(t *testing.T, configPath string, pairs ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("users:\n")
	for i := 0; i < len(pairs); i += 2 {
		fmt.Fprintf(&b, "  - name: %s\n    api_keys: [%s]\n", pairs[i], pairs[i+1])
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "users.yaml"), []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func readListen(t *testing.T, configPath string) string {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Listen string `yaml:"listen"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Listen
}

func waitProxyReady(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+address+"/accounting", nil)
		req.Header.Set("Authorization", "Bearer alice-key")
		resp, err := (&http.Client{Timeout: 150 * time.Millisecond}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy did not become ready at %s", address)
}

func waitListenerClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 30*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %s remained open after shutdown signal", address)
}

func create(address, key string) clientResult {
	return createWithTimeout(address, key, 3*time.Second)
}

func queryAccounting(address, key string) int {
	req, err := http.NewRequest(http.MethodGet, "http://"+address+"/accounting", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func createWithTimeout(address, key string, timeout time.Duration) clientResult {
	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","input":"synthetic"}`))
	if err != nil {
		return clientResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return clientResult{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return clientResult{status: resp.StatusCode}
}

func assertRequests(t *testing.T, stateDir, user string, want int64) {
	t.Helper()
	store, err := accounting.Open(filepath.Join(stateDir, "accounting.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := store.Query(context.Background(), accounting.Filters{User: user})
	if err != nil {
		t.Fatal(err)
	}
	got := int64(0)
	if report.Totals != nil {
		got, err = strconv.ParseInt(report.Totals.RequestCount.String(), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got != want {
		t.Fatalf("request count for %q = %d, want %d", user, got, want)
	}
}

func sqliteFile(path string) string {
	return "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(1000)"
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %04o, want %04o", path, got, want)
	}
}
