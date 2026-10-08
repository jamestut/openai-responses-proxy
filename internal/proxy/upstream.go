package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// targetURL preserves the configured upstream prefix and the caller's query and escaped path.
func (s *Server) targetURL(incoming *url.URL) *url.URL {
	target := *s.baseURL
	pathPrefix := strings.TrimRight(s.baseURL.Path, "/")
	escapedPrefix := strings.TrimRight(s.baseURL.EscapedPath(), "/")
	suffix := strings.TrimPrefix(incoming.Path, "/v1")
	escapedSuffix := strings.TrimPrefix(incoming.EscapedPath(), "/v1")
	target.Path = pathPrefix + suffix
	target.RawPath = escapedPrefix + escapedSuffix
	if target.RawPath == target.Path {
		target.RawPath = ""
	}
	target.RawQuery = incoming.RawQuery
	target.ForceQuery = incoming.ForceQuery
	target.Fragment = ""
	return &target
}

// doWithRetries retries only 429s; the bool means a request may have been sent before a transport error.
func (s *Server) doWithRetries(ctx context.Context, method string, target *url.URL, headers http.Header, body []byte) (*http.Response, error, bool) {
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			// Each attempt gets a fresh reader so retries replay the same body from byte zero.
			reader = bytes.NewReader(body)
		}
		var wroteRequest atomic.Bool
		trace := &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
		}
		request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, target.String(), reader)
		if err != nil {
			return nil, err, false
		}
		request.Header = cloneHeaders(headers)
		// Client credentials must never cross the trust boundary to the upstream.
		request.Header.Del("Authorization")
		request.Header.Del("Proxy-Authorization")
		request.Header.Set("Authorization", "Bearer "+s.config.Upstream.APIKey)
		if body != nil {
			request.ContentLength = int64(len(body))
		}
		response, err := s.client.Do(request)
		if err != nil {
			return response, err, wroteRequest.Load()
		}
		if response.StatusCode != http.StatusTooManyRequests || attempt >= s.config.Retry.MaxRetries {
			return response, nil, wroteRequest.Load()
		}
		// Close each retryable response before waiting so its body does not remain held open.
		_ = response.Body.Close()
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
			return response, ctx.Err(), false
		case <-timer.C:
		}
	}
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
