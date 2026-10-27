package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"openai-responses-proxy/internal/accounting"
	"openai-responses-proxy/internal/auth"
	"openai-responses-proxy/internal/config"
	"openai-responses-proxy/internal/logging"
)

// Server routes authenticated Responses requests through one configured upstream.
type Server struct {
	config     config.Config
	users      *auth.Users
	accounting *accounting.Store
	logger     *logging.Logger
	baseURL    *url.URL
	client     *http.Client
	transport  *http.Transport
	serviceCtx context.Context

	handlers sync.WaitGroup
}

// New builds the upstream client while retaining the standard TLS trust checks.
func New(cfg config.Config, users *auth.Users, store *accounting.Store, logger *logging.Logger, serviceCtx context.Context) (*Server, error) {
	baseURL, err := url.Parse(cfg.Upstream.BaseURL)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Upstream.CAFile != "" {
		// Add the private CA to system roots so public roots remain trusted too.
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA certificates: %w", err)
		}
		if roots == nil {
			return nil, errors.New("system CA certificate pool is unavailable")
		}
		certificates, err := os.ReadFile(cfg.Upstream.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream CA file: %w", err)
		}
		if ok := roots.AppendCertsFromPEM(certificates); !ok {
			return nil, errors.New("upstream.ca_file contains no PEM certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.Timeouts.Connect, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   cfg.Timeouts.TLSHandshake,
		ResponseHeaderTimeout: cfg.Timeouts.ResponseHeader,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		// Preserve the upstream wire body and headers so inspection and forwarding see the same bytes.
		DisableCompression: true,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// Surface redirects as upstream responses instead of following them implicitly.
			return http.ErrUseLastResponse
		},
	}
	return &Server{
		config: cfg, users: users, accounting: store, logger: logger,
		baseURL: baseURL, client: client, transport: transport,
		serviceCtx: serviceCtx,
	}, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Include authentication failures in handler draining during shutdown.
		s.handlers.Add(1)
		defer s.handlers.Done()
		user, ok := s.users.Authenticate(r)
		if !ok {
			s.logger.AuthenticationFailure()
			writeAPIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
			return
		}
		// Pin the authenticated identity now so a key reload cannot change in-flight attribution.
		s.serveHTTP(w, r, user)
	})
}

// WaitHandlers lets shutdown bound the time spent draining public requests.
func (s *Server) WaitHandlers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request, user string) {
	if r.URL.Path == "/accounting" {
		s.serveAccounting(w, r)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		writeAPIError(w, http.StatusNotFound, "unsupported route", "invalid_request_error")
		return
	}
	s.serveUpstream(w, r, user)
}

func (s *Server) serveAccounting(w http.ResponseWriter, r *http.Request) {
	// Any authenticated user can query global aggregates; the response never exposes raw rows.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	filters, err := accounting.ParseFilters(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	report, err := s.accounting.Query(r.Context(), filters)
	if err != nil {
		s.logger.Error("accounting query failed", "error", err.Error())
		writeAPIError(w, http.StatusServiceUnavailable, "accounting is unavailable", "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := jsonEncode(w, report); err != nil {
		s.logger.Error("write accounting query response", "error", err.Error())
	}
}

func (s *Server) Close() {
	s.transport.CloseIdleConnections()
}

func (s *Server) record(meta accounting.RequestMeta, result accounting.Result) {
	ctx, cancel := context.WithTimeout(s.serviceCtx, s.config.Timeouts.Shutdown)
	defer cancel()
	if err := s.accounting.Record(ctx, meta, result); err != nil {
		s.logger.Error("accounting record failed", "error", err.Error())
	}
}

func writeAPIError(w http.ResponseWriter, status int, message, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = jsonEncode(w, map[string]any{"error": map[string]any{"message": message, "type": kind}})
}

func jsonEncode(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}
