package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validConfigYAML = `upstream:
  base_url: http://upstream.example/proxy/v1
  api_key: mock-upstream-key
users_file: ./users.yaml
`

func writeConfig(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "proxy.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Loading a minimal config should apply documented defaults and resolve every relative file path beside that config.
func TestLoadDefaultsAndResolvesRelativePathsFromConfig(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	// Load checks that a configured CA path exists, so this placeholder only needs to be a local file.
	if err := os.WriteFile(caPath, []byte("synthetic CA fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	contents := `upstream:
  base_url: http://upstream.example/proxy/
  api_key: mock-upstream-key
  ca_file: ./ca.pem
users_file: ./users.yaml
state_dir: ./private-state
logging:
  destination: ./proxy.log
`
	cfg, err := Load(writeConfig(t, dir, contents))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" || cfg.Upstream.BaseURL != "http://upstream.example/proxy" || cfg.Upstream.APIKey != "mock-upstream-key" {
		t.Errorf("unexpected listen/upstream defaults: listen=%q base_url=%q api_key_present=%t", cfg.Listen, cfg.Upstream.BaseURL, cfg.Upstream.APIKey != "")
	}
	if cfg.UsersFile != filepath.Join(dir, "users.yaml") || cfg.StateDir != filepath.Join(dir, "private-state") || cfg.Upstream.CAFile != caPath || cfg.Logging.Destination != filepath.Join(dir, "proxy.log") {
		t.Errorf("relative paths were not resolved from config directory: users=%q state=%q ca=%q log=%q", cfg.UsersFile, cfg.StateDir, cfg.Upstream.CAFile, cfg.Logging.Destination)
	}
	if cfg.Retry.Delay != 30*time.Second || cfg.Retry.MaxRetries != 5 {
		t.Errorf("retry defaults = delay %s, max_retries %d", cfg.Retry.Delay, cfg.Retry.MaxRetries)
	}
	if cfg.Timeouts != (Timeouts{Connect: 10 * time.Second, TLSHandshake: 10 * time.Second, Shutdown: 30 * time.Second}) {
		t.Errorf("timeout defaults differ: connect=%s tls=%s header=%s total=%s shutdown=%s", cfg.Timeouts.Connect, cfg.Timeouts.TLSHandshake, cfg.Timeouts.ResponseHeader, cfg.Timeouts.TotalRequest, cfg.Timeouts.Shutdown)
	}
	if cfg.Logging.Level != "upstream_errors" || cfg.MaxRequestBodyBytes != 64*1024*1024 {
		t.Errorf("logging/body defaults differ: level=%q destination=%q body_limit=%d", cfg.Logging.Level, cfg.Logging.Destination, cfg.MaxRequestBodyBytes)
	}
}

// Explicit zero retry and request timeouts should remain zero, while absolute and relative paths keep their own bases.
func TestLoadExplicitZeroDurationsAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	absUsers := filepath.Join(dir, "absolute-users.yaml")
	contents := `listen: 127.0.0.1:12345
upstream:
  base_url: https://upstream.example/prefix/v1
  api_key: mock-key
users_file: ` + absUsers + `
state_dir: ./state
retry:
  delay: 0s
  max_retries: 0
timeouts:
  connect: 0s
  tls_handshake: 0s
  response_header: 0s
  total_request: 0s
  shutdown: 1ms
max_request_body_bytes: 1
logging:
  level: INFO
  destination: stdout
`
	cfg, err := Load(writeConfig(t, dir, contents))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsersFile != absUsers || cfg.StateDir != filepath.Join(dir, "state") {
		t.Errorf("paths = users %q, state %q", cfg.UsersFile, cfg.StateDir)
	}
	if cfg.Listen != "127.0.0.1:12345" || cfg.Upstream.BaseURL != "https://upstream.example/prefix/v1" || cfg.Logging.Level != "info" {
		t.Errorf("explicit values differ: listen=%q base_url=%q level=%q", cfg.Listen, cfg.Upstream.BaseURL, cfg.Logging.Level)
	}
	if cfg.Retry != (Retry{MaxRetries: 0}) || cfg.Timeouts != (Timeouts{Shutdown: time.Millisecond}) || cfg.MaxRequestBodyBytes != 1 {
		t.Errorf("explicit zero values were replaced: retry delay=%s max_retries=%d connect=%s tls=%s body=%d", cfg.Retry.Delay, cfg.Retry.MaxRetries, cfg.Timeouts.Connect, cfg.Timeouts.TLSHandshake, cfg.MaxRequestBodyBytes)
	}
}

// Strict decoding should reject misspelled, removed, and second-document settings instead of silently ignoring them.
func TestLoadRejectsUnknownRemovedAndMultipleDocumentFields(t *testing.T) {
	cases := []struct {
		name, contents string
	}{
		{"unknown_top_level", validConfigYAML + "legacy_database: ./accounting.sqlite\n"},
		{"removed_accounting_section", validConfigYAML + "accounting:\n  database: ./accounting.sqlite\n"},
		{"removed_background_poll_interval", validConfigYAML + "background:\n  poll_interval: 5s\n"},
		{"unknown_upstream_field", strings.Replace(validConfigYAML, "  api_key: mock-upstream-key", "  api_key: mock-upstream-key\n  verify_tls: false", 1)},
		{"unknown_retry_field", validConfigYAML + "retry:\n  delay: 0s\n  max_retries: 1\n  retry_after: true\n"},
		{"second_yaml_document", validConfigYAML + "---\nlisten: 127.0.0.1:8081\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, t.TempDir(), test.contents)); err == nil {
				t.Fatal("Load succeeded for unsupported YAML")
			}
		})
	}
}

// Invalid credentials, URLs, durations, limits, and logging settings should prevent the main config from loading.
func TestLoadRejectsInvalidMainConfiguration(t *testing.T) {
	cases := []struct {
		name, replace, with string
	}{
		{"empty_upstream_key", "api_key: mock-upstream-key", "api_key: '  '"},
		{"whitespace_upstream_key", "api_key: mock-upstream-key", "api_key: 'mock key'"},
		{"missing_users_file", "users_file: ./users.yaml", "users_file: ' '"},
		{"invalid_listen", "", "listen: 127.0.0.1:0\n"},
		{"negative_retries", "", "retry:\n  max_retries: -1\n"},
		{"zero_body_limit", "", "max_request_body_bytes: 0\n"},
		{"invalid_upstream_scheme", "http://upstream.example/proxy/v1", "ftp://upstream.example/proxy/v1"},
		{"upstream_query", "http://upstream.example/proxy/v1", "http://upstream.example/proxy/v1?secret=value"},
		{"upstream_credentials", "http://upstream.example/proxy/v1", "http://user:pass@upstream.example/proxy/v1"},
		{"negative_retry_delay", "", "retry:\n  delay: -1s\n"},
		{"invalid_retry_duration", "", "retry:\n  delay: next\n"},
		{"negative_connect_timeout", "", "timeouts:\n  connect: -1s\n"},
		{"negative_tls_timeout", "", "timeouts:\n  tls_handshake: -1s\n"},
		{"negative_response_header_timeout", "", "timeouts:\n  response_header: -1s\n"},
		{"negative_total_request_timeout", "", "timeouts:\n  total_request: -1s\n"},
		{"zero_shutdown_timeout", "", "timeouts:\n  shutdown: 0s\n"},
		{"negative_shutdown_timeout", "", "timeouts:\n  shutdown: -1s\n"},
		{"invalid_shutdown_duration", "", "timeouts:\n  shutdown: soon\n"},
		{"missing_ca_file", "api_key: mock-upstream-key", "api_key: mock-upstream-key\n  ca_file: ./missing-ca.pem"},
		{"invalid_log_level", "", "logging:\n  level: trace\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			contents := validConfigYAML
			if test.replace != "" {
				contents = strings.Replace(contents, test.replace, test.with, 1)
			} else {
				contents += test.with
			}
			if _, err := Load(writeConfig(t, t.TempDir(), contents)); err == nil {
				t.Fatal("Load succeeded for invalid config")
			}
		})
	}
}

// The example config and users file must each load successfully.
// Configured users and state paths must resolve beside the example config.
func TestLoadExampleConfigurationAndUsers(t *testing.T) {
	configPath := filepath.Join("..", "..", "examples", "proxy.example.yaml")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDir, err := filepath.Abs(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsersFile != filepath.Join(wantDir, "users.yaml") {
		t.Errorf("example users path = %q", cfg.UsersFile)
	}
	if cfg.StateDir != filepath.Join(wantDir, "state") {
		t.Errorf("example state path = %q", cfg.StateDir)
	}
	users, err := LoadUsers(filepath.Join(wantDir, "users.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Name != "alice" || users[1].Name != "bob" {
		t.Errorf("example user count or names differ (count=%d)", len(users))
	}
}

// Users loading should return a complete valid key set and reject duplicate, empty, or unsupported entries anywhere in the file.
func TestLoadUsersValidatesWholeFileAndRejectsRemovedFields(t *testing.T) {
	valid := `users:
  - name: alice
    api_keys: [alice-key, alice-rotation]
  - name: bob
    api_keys: [bob-key]
`
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	users, err := LoadUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []User{{Name: "alice", APIKeys: []string{"alice-key", "alice-rotation"}}, {Name: "bob", APIKeys: []string{"bob-key"}}}
	if !reflect.DeepEqual(users, want) {
		t.Fatalf("loaded user count or key counts differ (users=%d)", len(users))
	}

	cases := []struct {
		name, contents string
	}{
		{"empty_file", "users: []\n"},
		{"empty_name", "users:\n  - name: ' '\n    api_keys: [alice-key]\n"},
		{"duplicate_name", "users:\n  - name: alice\n    api_keys: [one]\n  - name: alice\n    api_keys: [two]\n"},
		{"no_keys", "users:\n  - name: alice\n    api_keys: []\n"},
		{"blank_key", "users:\n  - name: alice\n    api_keys: [' ']\n"},
		{"whitespace_key", "users:\n  - name: alice\n    api_keys: ['bad key']\n"},
		{"duplicate_key", "users:\n  - name: alice\n    api_keys: [same]\n  - name: bob\n    api_keys: [same]\n"},
		{"removed_pending_field", "users:\n  - name: alice\n    api_keys: [key]\n    pending: true\n"},
		{"unknown_user_field", "users:\n  - name: alice\n    api_keys: [key]\n    response_ids: []\n"},
		{"second_yaml_document", "users:\n  - name: alice\n    api_keys: [key]\n---\nusers: []\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadUsers(path); err == nil {
				t.Fatal("LoadUsers succeeded for invalid file")
			}
		})
	}
}
