package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// targetURL forwards the caller's query to the configured endpoint.
func (s *Server) targetURL(incoming *url.URL) *url.URL {
	target := *s.baseURL
	target.RawQuery = incoming.RawQuery
	target.ForceQuery = incoming.ForceQuery
	return &target
}

// doWithRetries retries only 429s.
func (s *Server) doWithRetries(ctx context.Context, target *url.URL, headers http.Header, body []byte) (*http.Response, error) {
	var previousResponse *http.Response
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			// Each attempt gets a fresh reader so retries replay the same body from byte zero.
			reader = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), reader)
		if err != nil {
			return nil, err
		}
		request.Header = cloneHeaders(headers)
		// Client credentials must never cross the trust boundary to the upstream.
		request.Header.Del("Authorization")
		request.Header.Del("Proxy-Authorization")
		if len(s.config.Upstream.HeaderCommand) == 0 {
			if s.config.Upstream.APIKey != "" {
				request.Header.Set("Authorization", "Bearer "+s.config.Upstream.APIKey)
			}
		} else if err := s.applyCommandHeaders(ctx, request.Header); err != nil {
			return previousResponse, err
		}
		if body != nil {
			request.ContentLength = int64(len(body))
		}
		response, err := s.client.Do(request)
		if err != nil {
			return response, err
		}
		if response.StatusCode != http.StatusTooManyRequests || attempt >= s.config.Retry.MaxRetries {
			return response, nil
		}
		// Close each retryable response before waiting so its body does not remain held open.
		_ = response.Body.Close()
		previousResponse = response
		// Retry-After is intentionally ignored; all attempts use the configured delay and jitter.
		delay := retryDelay(s.config.Retry.Delay)
		s.logger.Retry("upstream returned 429; retrying", "retry", attempt+1, "max_retries", s.config.Retry.MaxRetries, "delay", delay.String())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// The prior 429 is definitive; cancellation prevented the next attempt from being sent.
			return response, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Server) applyCommandHeaders(ctx context.Context, headers http.Header) error {
	command := s.config.Upstream.HeaderCommand
	output, err := exec.CommandContext(ctx, command[0], command[1:]...).Output()
	if err != nil {
		return errors.New("upstream header command failed")
	}
	var values map[string]string
	if err := json.Unmarshal(output, &values); err != nil || values == nil {
		return errors.New("upstream header command returned invalid JSON")
	}
	// Stable iteration prevents case-insensitive duplicate names from depending on map order.
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := values[name]
		canonical := http.CanonicalHeaderKey(name)
		if !validHeaderName(name) || !validHeaderValue(value) || canonical == "" || canonical == "Host" || hopByHopHeader(canonical) {
			continue
		}
		headers.Set(canonical, value)
	}
	return nil
}

func validHeaderValue(value string) bool {
	for _, r := range value {
		if (r < 32 && r != '\t') || r == 127 {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r > 127 || !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

// retryDelay applies independent +/- 20 percent jitter to each configured wait.
func retryDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(delay) * jitter)
}

func responseContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		// A disabled timeout still preserves cancellation from the request or service context.
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func cloneHeaders(source http.Header) http.Header {
	copy := make(http.Header, len(source))
	connectionTokens := make(map[string]struct{})
	// Connection can nominate additional headers that must be removed for this hop.
	for _, value := range source.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			connectionTokens[http.CanonicalHeaderKey(strings.TrimSpace(token))] = struct{}{}
		}
	}
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if hopByHopHeader(canonical) {
			continue
		}
		if _, excluded := connectionTokens[canonical]; excluded {
			continue
		}
		copy[canonical] = append([]string(nil), values...)
	}
	return copy
}

func copyResponseHeaders(destination, source http.Header) {
	filtered := cloneHeaders(source)
	for name, values := range filtered {
		destination[name] = append([]string(nil), values...)
	}
}

// hopByHopHeader lists fields that describe one transport connection, not the forwarded request.
func hopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func isClientGone(requestContext, operationContext context.Context, err error) bool {
	// Suppress a response only when the caller left; an operation timeout still gets a 504.
	if errors.Is(err, context.Canceled) && requestContext.Err() != nil {
		return true
	}
	return operationContext.Err() != nil && requestContext.Err() != nil
}
