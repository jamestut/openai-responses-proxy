package logging

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Default logging must include upstream, retry, and service failures but omit auth failures.
// Reopening at debug must add auth failures to the same private log file.
func TestUpstreamErrorsFilteringAndPrivateAppendFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "proxy.log")
	logger, err := New("upstream_errors", path)
	if err != nil {
		t.Fatal(err)
	}
	logger.AuthenticationFailure()
	logger.UpstreamError("upstream failed", "status", 500)
	logger.Retry("retrying", "attempt", 1)
	logger.Error("storage failed")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fileMode(t, path); got != 0600 {
		t.Fatalf("log file mode = %04o, want 0600", got)
	}
	if got := fileMode(t, filepath.Dir(path)); got != 0700 {
		t.Fatalf("created log directory mode = %04o, want 0700", got)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, want := range []string{"upstream failed", "retrying", "storage failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("log output missing %q", want)
		}
	}
	if strings.Contains(text, "downstream authentication failed") {
		t.Fatal("upstream_errors logged a downstream authentication failure")
	}

	logger, err = New("debug", path)
	if err != nil {
		t.Fatal(err)
	}
	logger.AuthenticationFailure()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "downstream authentication failed") {
		t.Fatal("debug logging omitted a downstream authentication failure")
	}
}

// The stdout destination writes logger output to the process stdout stream.
func TestStdoutDestination(t *testing.T) {
	oldStdout := os.Stdout
	// Redirect stdout temporarily so the test can assert the emitted line without changing logger setup.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	logger, err := New("error", "stdout")
	if err != nil {
		os.Stdout = oldStdout
		t.Fatal(err)
	}
	logger.Error("stdout destination")
	_ = logger.Close()
	os.Stdout = oldStdout
	_ = w.Close()
	output, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "stdout destination") {
		t.Fatalf("stdout output = %q", output)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
