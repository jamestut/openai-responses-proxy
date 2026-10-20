package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string      `yaml:"listen"`
	Upstream            Upstream    `yaml:"upstream"`
	UsersFile           string      `yaml:"users_file"`
	StateDir            string      `yaml:"state_dir"`
	Retry               Retry       `yaml:"retry"`
	Timeouts            Timeouts    `yaml:"timeouts"`
	Logging             Logging     `yaml:"logging"`
	MaxRequestBodyBytes int64       `yaml:"max_request_body_bytes"`
}

type Upstream struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	CAFile  string `yaml:"ca_file"`
}

type Retry struct {
	Delay      time.Duration
	MaxRetries int
}

type Timeouts struct {
	Connect        time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	TotalRequest   time.Duration
	Shutdown       time.Duration
}

type Logging struct {
	Level       string `yaml:"level"`
	Destination string `yaml:"destination"`
}

type rawConfig struct {
	Listen              string      `yaml:"listen"`
	Upstream            Upstream    `yaml:"upstream"`
	UsersFile           string      `yaml:"users_file"`
	StateDir            string      `yaml:"state_dir"`
	Retry               rawRetry    `yaml:"retry"`
	Timeouts            rawTimeouts `yaml:"timeouts"`
	Logging             Logging     `yaml:"logging"`
	MaxRequestBodyBytes int64       `yaml:"max_request_body_bytes"`
}

type rawRetry struct {
	Delay      string `yaml:"delay"`
	MaxRetries int    `yaml:"max_retries"`
}

type rawTimeouts struct {
	Connect        string `yaml:"connect"`
	TLSHandshake   string `yaml:"tls_handshake"`
	ResponseHeader string `yaml:"response_header"`
	TotalRequest   string `yaml:"total_request"`
	Shutdown       string `yaml:"shutdown"`
}

type rawUsers struct {
	Users []User `yaml:"users"`
}

type User struct {
	Name    string   `yaml:"name"`
	APIKeys []string `yaml:"api_keys"`
}

// Load applies defaults, validates the main YAML file, and resolves its paths from that file's directory.
func Load(path string) (Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	// Prepopulate defaults so omitted fields inherit them while explicit zero values retain their configured meaning.
	raw := rawConfig{
		Listen:              "127.0.0.1:8080",
		StateDir:            "./var",
		Retry:               rawRetry{Delay: "30s", MaxRetries: 5},
		Timeouts:            rawTimeouts{Connect: "10s", TLSHandshake: "10s", ResponseHeader: "0s", TotalRequest: "0s", Shutdown: "30s"},
		Logging:             Logging{Level: "upstream_errors", Destination: "stdout"},
		MaxRequestBodyBytes: 64 * 1024 * 1024,
	}
	if err := decodeStrict(data, &raw); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	// The upstream key becomes one Bearer credential, so whitespace would change its wire representation.
	if strings.TrimSpace(raw.Upstream.APIKey) == "" || strings.ContainsAny(raw.Upstream.APIKey, " \t\r\n") {
		return Config{}, errors.New("upstream.api_key must be a nonempty bearer token without whitespace")
	}
	baseURL, err := parseUpstreamURL(raw.Upstream.BaseURL)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(raw.UsersFile) == "" {
		return Config{}, errors.New("users_file is required")
	}
	if !validListenAddress(raw.Listen) {
		return Config{}, fmt.Errorf("invalid listen address %q", raw.Listen)
	}
	if raw.Retry.MaxRetries < 0 {
		return Config{}, errors.New("retry.max_retries must be zero or greater")
	}
	// Reject unbounded request sizes before any service components start.
	if raw.MaxRequestBodyBytes <= 0 {
		return Config{}, errors.New("max_request_body_bytes must be positive")
	}

	// All relative paths use the config location, independent of the process working directory.
	baseDir := filepath.Dir(absolute)
	usersFile := resolvePath(baseDir, raw.UsersFile)
	stateDir := resolvePath(baseDir, raw.StateDir)
	caFile := ""
	if raw.Upstream.CAFile != "" {
		caFile = resolvePath(baseDir, raw.Upstream.CAFile)
		if _, err := os.Stat(caFile); err != nil {
			return Config{}, fmt.Errorf("upstream.ca_file: %w", err)
		}
	}
	destination := raw.Logging.Destination
	if destination == "" || destination == "stdout" {
		destination = "stdout"
	} else {
		destination = resolvePath(baseDir, destination)
	}
	level := strings.ToLower(raw.Logging.Level)
	switch level {
	case "upstream_errors", "error", "warn", "info", "debug":
	default:
		return Config{}, fmt.Errorf("unsupported logging.level %q", raw.Logging.Level)
	}

	// Zero retry delay and transport/request timeouts are allowed when callers want no wait or no deadline.
	delay, err := time.ParseDuration(raw.Retry.Delay)
	if err != nil || delay < 0 {
		return Config{}, fmt.Errorf("retry.delay must be a nonnegative Go duration")
	}
	connect, err := time.ParseDuration(raw.Timeouts.Connect)
	if err != nil || connect < 0 {
		return Config{}, errors.New("timeouts.connect must be a nonnegative Go duration")
	}
	tlsHandshake, err := time.ParseDuration(raw.Timeouts.TLSHandshake)
	if err != nil || tlsHandshake < 0 {
		return Config{}, errors.New("timeouts.tls_handshake must be a nonnegative Go duration")
	}
	responseHeader, err := time.ParseDuration(raw.Timeouts.ResponseHeader)
	if err != nil || responseHeader < 0 {
		return Config{}, errors.New("timeouts.response_header must be a nonnegative Go duration")
	}
	totalRequest, err := time.ParseDuration(raw.Timeouts.TotalRequest)
	if err != nil || totalRequest < 0 {
		return Config{}, errors.New("timeouts.total_request must be a nonnegative Go duration")
	}
	// Shutdown draining requires a positive deadline.
	shutdown, err := time.ParseDuration(raw.Timeouts.Shutdown)
	if err != nil || shutdown <= 0 {
		return Config{}, errors.New("timeouts.shutdown must be a positive Go duration")
	}
	return Config{
		Listen:              raw.Listen,
		Upstream:            Upstream{BaseURL: baseURL.String(), APIKey: raw.Upstream.APIKey, CAFile: caFile},
		UsersFile:           usersFile,
		StateDir:            stateDir,
		Retry:               Retry{Delay: delay, MaxRetries: raw.Retry.MaxRetries},
		Timeouts:            Timeouts{Connect: connect, TLSHandshake: tlsHandshake, ResponseHeader: responseHeader, TotalRequest: totalRequest, Shutdown: shutdown},
		Logging:             Logging{Level: level, Destination: destination},
		MaxRequestBodyBytes: raw.MaxRequestBodyBytes,
	}, nil
}

// LoadUsers returns a complete validated key set; callers can retain the active set on any error.
func LoadUsers(path string) ([]User, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read users file: %w", err)
	}
	var raw rawUsers
	if err := decodeStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("decode users file: %w", err)
	}
	if len(raw.Users) == 0 {
		return nil, errors.New("users file must define at least one user")
	}
	// Track identities and keys across the whole file to prevent ambiguous authentication results.
	users := make([]User, 0, len(raw.Users))
	userNames := make(map[string]struct{}, len(raw.Users))
	keys := make(map[string]string)
	for _, item := range raw.Users {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return nil, errors.New("user name must not be empty")
		}
		if _, exists := userNames[name]; exists {
			return nil, fmt.Errorf("duplicate user name %q", name)
		}
		userNames[name] = struct{}{}
		if len(item.APIKeys) == 0 {
			return nil, fmt.Errorf("user %q must have at least one api key", name)
		}
		user := User{Name: name, APIKeys: make([]string, 0, len(item.APIKeys))}
		for _, key := range item.APIKeys {
			if strings.TrimSpace(key) == "" || strings.ContainsAny(key, " \t\r\n") {
				return nil, fmt.Errorf("user %q has an empty or invalid api key", name)
			}
			if previous, exists := keys[key]; exists {
				return nil, fmt.Errorf("api key is duplicated for users %q and %q", previous, name)
			}
			keys[key] = name
			user.APIKeys = append(user.APIKeys, key)
		}
		users = append(users, user)
	}
	return users, nil
}

func resolvePath(baseDir, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func parseUpstreamURL(value string) (*url.URL, error) {
	// Reject URL components that conflict with per-request route joining or could embed credentials.
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("upstream.base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("upstream.base_url scheme must be http or https")
	}
	// Keep any upstream path prefix while normalizing only trailing slashes around the required /v1 suffix.
	cleanPath := strings.TrimRight(parsed.Path, "/")
	if cleanPath != "/v1" && !strings.HasSuffix(cleanPath, "/v1") {
		return nil, errors.New("upstream.base_url path must end in /v1")
	}
	parsed.Path = cleanPath
	parsed.RawPath = ""
	return parsed, nil
}

func validListenAddress(value string) bool {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.TrimSpace(host) != host {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535
}

func decodeStrict(data []byte, output any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	// Reject unknown keys so misspelled security or timeout settings cannot silently take defaults.
	decoder.KnownFields(true)
	if err := decoder.Decode(output); err != nil {
		return err
	}
	// A second YAML document would make file interpretation ambiguous, so require EOF after the first.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple YAML documents are not supported")
		}
		return err
	}
	return nil
}
