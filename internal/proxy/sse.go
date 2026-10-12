package proxy

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"

	"openai-responses-proxy/internal/accounting"
)

// serveSSE forwards complete events promptly while observing Responses usage for accounting.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, response *http.Response, requestID accounting.RequestID, isAccounted bool) {
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	last := responseInfo{ID: responseIDFromPath(r.URL.Path)}
	terminal := false
	clientWriteFailed := false
	reader := bufio.NewReader(response.Body)
	var eventLines [][]byte
	var dataLines []string
	eventName := ""
	var streamErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, ":") && len(eventLines) == 0 && len(dataLines) == 0 {
				// Forward standalone SSE comments as heartbeats without buffering the next event.
				if _, writeErr := w.Write(line); writeErr != nil {
					clientWriteFailed = true
					streamErr = writeErr
					break
				}
				if canFlush {
					flusher.Flush()
				}
			} else {
				// Hold fields until the blank delimiter; partial frames cannot change accounting.
				eventLines = append(eventLines, append([]byte(nil), line...))
				if trimmed == "" {
					s.observeSSEEvent(eventName, dataLines, &last, &terminal, response.StatusCode, requestID, isAccounted)
					if _, writeErr := writeEvent(w, eventLines); writeErr != nil {
						clientWriteFailed = true
						streamErr = writeErr
						break
					}
					if canFlush {
						flusher.Flush()
					}
					eventLines = nil
					dataLines = nil
					eventName = ""
				} else {
					field, value, found := strings.Cut(strings.TrimRight(string(line), "\r\n"), ":")
					// SSE removes at most one optional space after the colon; further spaces are data.
					if found && len(value) > 0 && value[0] == ' ' {
						value = value[1:]
					}
					switch field {
					case "event":
						eventName = value
					case "data":
						dataLines = append(dataLines, value)
					}
				}
			}
		}
		if err != nil {
			streamErr = err
			if !errors.Is(err, io.EOF) {
				break
			}
			if len(eventLines) > 0 {
				// Preserve the frame and its ID for a later client retrieval; partial events cannot update usage.
				if info, ok := parseSSEEvent(eventName, dataLines); ok && info.ID != "" {
					last.ID = info.ID
					if isAccounted {
						s.attachResponseID(requestID, info.ID)
					}
				}
				if _, writeErr := writeEvent(w, eventLines); writeErr != nil {
					clientWriteFailed = true
					streamErr = writeErr
				}
			}
			break
		}
	}

	if isAccounted && !terminal {
		// Keep a row with an ID pending so a later client retrieval can observe its final state.
		if response.StatusCode >= 400 {
			last.Status = "failed"
			s.finalize(requestID, last, response.StatusCode, "failed")
		} else if last.ID == "" {
			reason := "SSE stream ended before a terminal Responses event"
			if clientWriteFailed {
				reason = "client disconnected before a terminal Responses event"
			} else if streamErr != nil && !errors.Is(streamErr, io.EOF) {
				reason = "SSE stream ended before final data"
			}
			s.markIncomplete(requestID, reason)
		}
	}
	if streamErr != nil && !errors.Is(streamErr, io.EOF) && !clientWriteFailed {
		s.logger.UpstreamError("upstream SSE stream ended early", "error", streamErr.Error())
	}
}

func (s *Server) observeSSEEvent(eventName string, dataLines []string, last *responseInfo, terminal *bool, httpStatus int, requestID accounting.RequestID, isAccounted bool) {
	info, hasResponse := parseSSEEvent(eventName, dataLines)
	if !hasResponse {
		return
	}
	if info.ID == "" {
		// Some event payloads omit the ID after the initial response.created event.
		info.ID = last.ID
	}
	*last = info
	if isAccounted && info.ID != "" && !*terminal {
		s.attachResponseID(requestID, info.ID)
	}
	if isTerminalStatus(info.Status) {
		// Terminal status closes the accounting lifecycle even if earlier events carried usage.
		*terminal = true
		if isAccounted {
			outcome := info.Status
			if httpStatus >= 400 {
				outcome = "failed"
			}
			s.finalize(requestID, info, httpStatus, outcome)
		} else if info.ID != "" {
			s.observeResponse(info, httpStatus)
		}
		return
	}
	if info.ID != "" && info.UsageKnown {
		// Retain the latest known usage so a later terminal event without usage does not erase it.
		snapshot := info.snapshot(accounting.PhaseProvisional, httpStatus, "")
		if _, err := s.accounting.ObserveByResponseID(s.persistContext(), info.ID, snapshot); err != nil {
			s.logger.Error("could not observe streamed response usage", "error", err.Error())
		}
	}
}

func parseSSEEvent(eventName string, dataLines []string) (responseInfo, bool) {
	if len(dataLines) == 0 {
		return responseInfo{}, false
	}
	info := parseResponse([]byte(strings.Join(dataLines, "\n")))
	if !info.Valid {
		return responseInfo{}, false
	}
	if info.Status == "" {
		// The event name supplies lifecycle status when the embedded Response omits it.
		switch eventName {
		case "response.completed":
			info.Status = "completed"
		case "response.failed":
			info.Status = "failed"
		case "response.incomplete":
			info.Status = "incomplete"
		case "response.cancelled", "response.canceled":
			info.Status = "cancelled"
		case "response.created", "response.in_progress":
			info.Status = "in_progress"
		}
	}
	return info, true
}

func writeEvent(w http.ResponseWriter, lines [][]byte) (int64, error) {
	var written int64
	for _, line := range lines {
		n, err := w.Write(line)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
