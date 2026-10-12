package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openai-responses-proxy/internal/accounting"
)

// serveUpstream handles one accepted route while keeping retries on the same request bytes.
func (s *Server) serveUpstream(w http.ResponseWriter, r *http.Request, kind accounting.Kind) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	// Record arrival time before body reading or accounting admission can add delay.
	startedAt := time.Now().UTC()
	body, err := readRequestBody(r, s.config.MaxRequestBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestTooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error")
		} else if r.Context().Err() == nil {
			writeAPIError(w, http.StatusBadRequest, "could not read request body", "invalid_request_error")
		}
		return
	}

	// The optional operation deadline is layered on the client context so disconnects still cancel work.
	requestContext, cancel := responseContext(r.Context(), s.config.Timeouts.TotalRequest)
	defer cancel()
	model, background := requestOptions(body)
	var requestID accounting.RequestID
	isAccounted := kind == accounting.KindCreate || kind == accounting.KindCompact
	if isAccounted {
		// Admit once before upstream work; retries and later client retrieval update this same row.
		meta := accounting.RequestMeta{
			StartedAt: startedAt, User: userFromContext(r.Context()),
			Model: model, Kind: kind,
		}
		requestID, err = s.accounting.Admit(s.persistContext(), meta)
		if err != nil {
			s.logger.Error("accounting admission failed", "error", err.Error())
			writeAPIError(w, http.StatusServiceUnavailable, "accounting is unavailable", "server_error")
			return
		}
	}

	target := s.targetURL(r.URL)
	response, roundTripErr, wroteRequest := s.doWithRetries(requestContext, r.Method, target, r.Header, body)
	if roundTripErr != nil {
		if isAccounted {
			// Distinguish a definitive response from a possibly sent request whose result was lost.
			switch {
			case response != nil && response.StatusCode == http.StatusTooManyRequests:
				s.finalize(requestID, responseInfo{Status: "failed", Valid: true}, http.StatusTooManyRequests, "failed")
			case wroteRequest:
				s.markIncomplete(requestID, "upstream transport ended after the request may have been sent")
			default:
				s.finalize(requestID, responseInfo{Status: "failed", Valid: true}, 0, "failed")
			}
		}
		if isClientGone(r.Context(), requestContext, roundTripErr) {
			return
		}
		s.logger.UpstreamError("upstream request failed", "error", roundTripErr.Error())
		if errors.Is(roundTripErr, context.DeadlineExceeded) || requestContext.Err() == context.DeadlineExceeded {
			writeAPIError(w, http.StatusGatewayTimeout, "upstream request timed out", "server_error")
		} else {
			writeAPIError(w, http.StatusBadGateway, "upstream request failed", "server_error")
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		s.logger.UpstreamError("upstream returned 429 after retries", "status", response.StatusCode)
	} else if response.StatusCode >= 400 {
		// Log route metadata only; response IDs, query strings, and bodies can be sensitive.
		s.logger.UpstreamError("upstream returned an HTTP error",
			"status", response.StatusCode,
			"method", r.Method,
			"path", sanitizedUpstreamPath(r.URL.Path),
			"category", upstreamHTTPErrorCategory(response.StatusCode))
	}

	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		// Streams are forwarded incrementally so clients see events before the upstream finishes.
		s.serveSSE(w, r, response, requestID, isAccounted)
		return
	}
	// Buffer JSON once to inspect accounting fields while still forwarding the original bytes.
	responseBody, readErr := io.ReadAll(response.Body)
	info := parseResponse(responseBody)
	if info.ID == "" {
		info.ID = responseIDFromPath(r.URL.Path)
	}
	if response.StatusCode >= 400 && info.ID != "" && info.UsageKnown && !info.Valid {
		// A retrieve error may omit Response metadata; its path ID and usage still identify failure.
		info.Valid = true
		info.Status = "failed"
	}
	if isAccounted {
		if response.StatusCode < 400 {
			if readErr != nil {
				s.markIncomplete(requestID, "upstream response body ended before final data")
			} else {
				s.accountResponse(requestID, response.StatusCode, info, background)
			}
		} else {
			// Error envelopes can carry standard usage even when they are not Response objects.
			if !info.UsageKnown {
				info.Usage, info.UsageKnown = accounting.ParseUsage(responseBody)
			}
			s.finalize(requestID, info, response.StatusCode, "failed")
		}
	} else if info.Valid && info.ID != "" {
		s.observeResponse(info, response.StatusCode)
	}

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if len(responseBody) > 0 {
		_, _ = io.Copy(w, bytes.NewReader(responseBody))
	}
	if readErr != nil {
		s.logger.UpstreamError("upstream response body ended early", "error", readErr.Error())
	}
}

func upstreamHTTPErrorCategory(status int) string {
	switch {
	case status >= 500:
		return "server_error"
	case status >= 400:
		return "client_error"
	default:
		return "http_error"
	}
}

// sanitizedUpstreamPath removes response IDs before route metadata reaches logs.
func sanitizedUpstreamPath(path string) string {
	switch path {
	case "/v1/responses", "/v1/responses/compact", "/v1/responses/input_tokens":
		return path
	}
	if !strings.HasPrefix(path, "/v1/responses/") {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/responses/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		return "/v1/responses/{response_id}"
	}
	if len(parts) == 2 && parts[0] != "" {
		switch parts[1] {
		case "cancel", "input_items":
			return "/v1/responses/{response_id}/" + parts[1]
		}
	}
	return "/v1/responses/{response_id}"
}

var errRequestTooLarge = errors.New("request body exceeds configured limit")

func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	// Read one byte beyond the limit to distinguish an exact fit from an oversized body.
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errRequestTooLarge
	}
	return body, nil
}

func requestOptions(body []byte) (string, bool) {
	// Metadata extraction is best-effort; the original JSON remains the forwarded request body.
	var request struct {
		Model      string `json:"model"`
		Background bool   `json:"background"`
	}
	if json.Unmarshal(body, &request) != nil {
		return "", false
	}
	return request.Model, request.Background
}

// accountResponse connects the admitted row to its response ID or a final outcome.
func (s *Server) accountResponse(id accounting.RequestID, status int, info responseInfo, background bool) {
	if info.ID != "" {
		if err := s.accounting.AttachResponseID(s.persistContext(), id, info.ID); err != nil {
			s.logger.Error("could not attach upstream response ID", "error", err.Error())
		}
	}
	if info.Valid && isTerminalStatus(info.Status) {
		// Terminal Responses states can close the admitted row immediately.
		s.finalize(id, info, status, info.Status)
		return
	}
	if info.Valid && info.ID != "" && (background || isPendingStatus(info.Status)) {
		// Keep the row pending so a later client retrieval can observe its final response.
		return
	}
	if info.Valid && info.ID != "" {
		// Preserve the existing incomplete classification for unknown foreground states.
		s.markIncomplete(id, "upstream returned a nonterminal response that cannot be recovered")
		return
	}
	// Without a usable object or ID, a later client retrieval cannot update the row.
	s.markIncomplete(id, "upstream response did not contain a usable Responses object")
}

func (s *Server) observeResponse(info responseInfo, httpStatus int) {
	// Non-admission routes update an existing row by upstream ID and never add another row.
	phase := accounting.PhaseProvisional
	outcome := ""
	if isTerminalStatus(info.Status) {
		phase = accounting.PhaseFinal
		outcome = info.Status
	}
	snapshot := info.snapshot(phase, httpStatus, outcome)
	if _, err := s.accounting.ObserveByResponseID(s.persistContext(), info.ID, snapshot); err != nil {
		s.logger.Error("could not update observed response usage", "error", err.Error())
	}
}

func (s *Server) finalize(id accounting.RequestID, info responseInfo, httpStatus int, outcome string) {
	phaseOutcome := outcome
	if phaseOutcome == "" {
		phaseOutcome = "completed"
	}
	if err := s.accounting.Finalize(s.persistContext(), id, info.snapshot(accounting.PhaseFinal, httpStatus, phaseOutcome)); err != nil {
		s.logger.Error("could not finalize accounting record", "error", err.Error())
	}
}

func (s *Server) markIncomplete(id accounting.RequestID, reason string) {
	if err := s.accounting.MarkIncomplete(s.persistContext(), id, "", reason); err != nil {
		s.logger.Error("could not mark accounting record incomplete", "error", err.Error())
	}
}

func (s *Server) persistContext() context.Context {
	// Persistence follows service lifetime so a client disconnect cannot cancel durable updates.
	return s.serviceCtx
}

// responseIDFromPath extracts only a single response path segment as an ID fallback.
func responseIDFromPath(path string) string {
	if !strings.HasPrefix(path, "/v1/responses/") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/responses/"), "/")
	if len(parts) != 1 {
		return ""
	}
	return parts[0]
}

func (s *Server) attachResponseID(id accounting.RequestID, responseID string) {
	if responseID == "" {
		return
	}
	if err := s.accounting.AttachResponseID(s.persistContext(), id, responseID); err != nil {
		s.logger.Error("could not attach upstream response ID", "error", fmt.Sprint(err))
	}
}
