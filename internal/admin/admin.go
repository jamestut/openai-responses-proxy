package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"openai-responses-proxy/internal/auth"
	"openai-responses-proxy/internal/config"
	"openai-responses-proxy/internal/logging"
)

const socketName = "reload.sock"

type Server struct {
	server    *http.Server
	listener  net.Listener
	path      string
	users     *auth.Users
	usersFile string
	logger    *logging.Logger
}

type response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Start exposes key reload over a private Unix socket inside stateDir.
func Start(stateDir, usersFile string, users *auth.Users, logger *logging.Logger) (*Server, error) {
	// The control endpoint can replace credentials, so its socket lives in the private state directory.
	if err := EnsureStateDir(stateDir); err != nil {
		return nil, err
	}
	socketPath := filepath.Join(stateDir, socketName)
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on reload socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		os.Remove(socketPath)
		return nil, fmt.Errorf("set reload socket permissions: %w", err)
	}
	// Serve only the reload operation; the fallback prevents accidental control routes from being exposed.
	control := &Server{listener: listener, path: socketPath, users: users, usersFile: usersFile, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reload", control.reload)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	control.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := control.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("administration socket stopped", "error", err.Error())
		}
	}()
	return control, nil
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 0 {
		writeResponse(w, http.StatusBadRequest, response{Error: "reload request must have an empty body"})
		return
	}
	// Validate the entire replacement before publishing it, preserving the active snapshot on failure.
	users, err := config.LoadUsers(s.usersFile)
	if err != nil {
		writeResponse(w, http.StatusBadRequest, response{Error: err.Error()})
		return
	}
	s.users.Replace(users)
	writeResponse(w, http.StatusOK, response{OK: true})
}

func (s *Server) Close(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	// Remove the filesystem entry after shutdown so a future start sees a clean state directory.
	if closeErr := s.listener.Close(); err == nil && closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		err = closeErr
	}
	if removeErr := os.Remove(s.path); err == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = removeErr
	}
	return err
}

// Reload asks the running service to validate and atomically activate its current users file.
func Reload(stateDir string) error {
	socketPath := filepath.Join(stateDir, socketName)
	// Route HTTP through a Unix dialer; the synthetic host is never resolved over the network.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://unix/reload", nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	result, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("contact running proxy: %w", err)
	}
	defer result.Body.Close()
	var body response
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		return fmt.Errorf("read reload result: %w", err)
	}
	if result.StatusCode != http.StatusOK || !body.OK {
		message := strings.TrimSpace(body.Error)
		if message == "" {
			message = result.Status
		}
		return fmt.Errorf("key reload failed: %s", message)
	}
	return nil
}

// EnsureStateDir creates a private directory and rejects a symlink at its final path.
func EnsureStateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	// Lstat catches a symlink at the final path before permissions are tightened.
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("state directory must be a real directory")
	}
	return os.Chmod(path, 0700)
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace a non-socket reload path")
	}
	// A successful connection means another instance owns the path; only refused/missing sockets are stale.
	conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		return errors.New("another proxy is already using the reload socket")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("cannot determine whether reload socket is stale: %w", dialErr)
	}
	return os.Remove(path)
}

func writeResponse(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
