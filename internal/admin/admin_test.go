package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openai-responses-proxy/internal/auth"
	"openai-responses-proxy/internal/config"
	"openai-responses-proxy/internal/logging"
)

// Reload accepts a complete valid users file, rejects duplicates without replacing active keys, and uses a private socket.
func TestReloadActivatesOnlyValidUsersAndUsesPrivateSocket(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := EnsureStateDir(stateDir); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, stateDir); got != 0700 {
		t.Fatalf("state directory mode = %04o, want 0700", got)
	}
	usersPath := filepath.Join(t.TempDir(), "users.yaml")
	writeUsers(t, usersPath, "alice", "alice-old")
	initial, err := config.LoadUsers(usersPath)
	if err != nil {
		t.Fatal(err)
	}
	active := auth.NewUsers(initial)
	logger, err := logging.New("error", "stdout")
	if err != nil {
		t.Fatal(err)
	}
	control, err := Start(stateDir, usersPath, active, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = control.Close(ctx)
	})
	socketPath := filepath.Join(stateDir, socketName)
	if got := mode(t, socketPath); got != 0600 {
		t.Fatalf("reload socket mode = %04o, want 0600", got)
	}

	writeUsers(t, usersPath, "alice", "alice-new")
	// A valid replacement activates the new key and removes the old one.
	if err := Reload(stateDir); err != nil {
		t.Fatalf("valid reload: %v", err)
	}
	if who := authenticate(active, "alice-new"); who != "alice" {
		t.Fatalf("new key authenticated as %q", who)
	}
	if who := authenticate(active, "alice-old"); who != "" {
		t.Fatalf("removed key still authenticated as %q", who)
	}

	if err := os.WriteFile(usersPath, []byte("users:\n  - name: alice\n    api_keys: [same]\n  - name: bob\n    api_keys: [same]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reload(stateDir); err == nil {
		t.Fatal("reload with duplicate key succeeded")
	}
	// Failed whole-file validation leaves the last valid snapshot active.
	if who := authenticate(active, "alice-new"); who != "alice" {
		t.Fatalf("failed reload replaced the active map; alice-new authenticated as %q", who)
	}
	if who := authenticate(active, "same"); who != "" {
		t.Fatalf("failed reload activated duplicate key as %q", who)
	}

	if err := postReload(stateDir, "unexpected"); err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("nonempty reload body error = %v, want empty-body rejection", err)
	}
}

// State setup rejects a symlink directory, and startup preserves a regular file at the reload socket path.
func TestEnsureStateDirRejectsSymlinkAndReloadRejectsNonSocket(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := EnsureStateDir(symlink); err == nil {
		t.Fatal("EnsureStateDir accepted a symlink")
	}

	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, socketName), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	logger, err := logging.New("error", "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(stateDir, filepath.Join(root, "users.yaml"), auth.NewUsers(nil), logger); err == nil {
		t.Fatal("Start replaced a non-socket reload path")
	}
}

func postReload(stateDir, body string) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(stateDir, socketName))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://unix/reload", strings.NewReader(body))
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return nil
	}
	return readReloadError(response)
}

func readReloadError(response *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return err
	}
	return errors.New(body.Error)
}

func writeUsers(t *testing.T, path, name, key string) {
	t.Helper()
	data := "users:\n  - name: " + name + "\n    api_keys: [" + key + "]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func authenticate(users *auth.Users, key string) string {
	request, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	name, ok := users.Authenticate(request)
	if !ok {
		return ""
	}
	return name
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
