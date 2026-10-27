package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"openai-responses-proxy/internal/accounting"
)

// serveUpstream handles one accepted route while keeping retries on the same request bytes.
func (s *Server) serveUpstream(w http.ResponseWriter, r *http.Request, user string) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	startedAt := time.Now().UTC()
	meta := accounting.RequestMeta{StartedAt: startedAt, User: user}
	result := accounting.Result{Outcome: "incomplete", IncompleteReason: accounting.ReasonInvalidResponse}
	// The result is inserted once on exit, even if the client disconnects.
	defer func() { s.record(meta, result) }()

	body, err := readRequestBody(r, s.config.MaxRequestBodyBytes)
	if err != nil {
		// These are local rejections: http_status stays zero because no upstream replied.
		if errors.Is(err, errRequestTooLarge) {
			result.Outcome = "failed"
			result.IncompleteReason = accounting.ReasonNone
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error")
		} else if r.Context().Err() == nil {
			result.Outcome = "failed"
			result.IncompleteReason = accounting.ReasonNone
			writeAPIError(w, http.StatusBadRequest, "could not read request body", "invalid_request_error")
		} else {
			result.IncompleteReason = accounting.ReasonClientDisconnected
		}
		return
	}
	meta.Model = requestModel(body)

	requestContext, cancel := responseContext(r.Context(), s.config.Timeouts.TotalRequest)
	defer cancel()
	target := s.targetURL(r.URL)
	response, roundTripErr := s.doWithRetries(requestContext, target, r.Header, body)
	if roundTripErr != nil {
		result = accounting.Result{Outcome: "incomplete", IncompleteReason: accounting.ReasonTransportError}
		if response != nil {
			result.HTTPStatus = response.StatusCode
			if response.StatusCode == http.StatusTooManyRequests {
				result.Status = "failed"
				result.Outcome = "failed"
				result.IncompleteReason = accounting.ReasonNone
			}
		}
		if isClientGone(r.Context(), requestContext, roundTripErr) {
			result.IncompleteReason = accounting.ReasonClientDisconnected
			return
		}
		// url.Error can include the full target, including query values; keep transport logs fixed.
		s.logger.UpstreamError("upstream request failed", "error", "transport error")
		if errors.Is(roundTripErr, context.DeadlineExceeded) || requestContext.Err() == context.DeadlineExceeded {
			writeAPIError(w, http.StatusGatewayTimeout, "upstream request timed out", "server_error")
		} else {
			writeAPIError(w, http.StatusBadGateway, "upstream request failed", "server_error")
		}
		return
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode == http.StatusTooManyRequests {
		s.logger.UpstreamError("upstream returned 429 after retries", "status", response.StatusCode)
	} else if response.StatusCode >= 400 {
		s.logger.UpstreamError("upstream returned an HTTP error",
			"status", response.StatusCode,
			"method", r.Method,
			"path", r.URL.Path,
			"category", upstreamHTTPErrorCategory(response.StatusCode))
	}

	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		result = s.serveSSE(w, r.Context(), response)
		result.HTTPStatus = response.StatusCode
		return
	}
	responseBody, readErr := io.ReadAll(response.Body)
	info := parseResponse(responseBody)
	if response.StatusCode >= 400 && info.Status == "" {
		info.Status = "failed"
	}
	result = responseResult(info, response.StatusCode)
	if r.Context().Err() != nil {
		result.Outcome = "incomplete"
		result.IncompleteReason = accounting.ReasonClientDisconnected
	} else if readErr != nil {
		result.Outcome = "incomplete"
		result.IncompleteReason = accounting.ReasonResponseReadError
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if len(responseBody) > 0 {
		_, _ = io.Copy(w, bytes.NewReader(responseBody))
	}
	if readErr != nil && r.Context().Err() == nil {
		s.logger.UpstreamError("upstream response body ended early", "error", "response read error")
	}
}

func responseResult(info responseInfo, httpStatus int) accounting.Result {
	result := accounting.Result{
		ResponseModel:    info.Model,
		Status:           info.Status,
		HTTPStatus:       httpStatus,
		Usage:            info.Usage,
		Outcome:          "incomplete",
		IncompleteReason: accounting.ReasonNonterminalResponse,
	}
	if httpStatus >= 400 || info.Status == "failed" {
		result.Outcome = "failed"
		result.IncompleteReason = accounting.ReasonNone
	} else if isTerminalStatus(info.Status) {
		result.Outcome = info.Status
		result.IncompleteReason = accounting.ReasonNone
	} else if !info.Valid {
		result.IncompleteReason = accounting.ReasonInvalidResponse
	}
	return result
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

var errRequestTooLarge = errors.New("request body exceeds configured limit")

func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errRequestTooLarge
	}
	return body, nil
}

func requestModel(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return request.Model
}
