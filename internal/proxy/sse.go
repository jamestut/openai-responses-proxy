package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"openai-responses-proxy/internal/accounting"
)

// serveSSE forwards raw event frames while accounting only complete terminal events.
func (s *Server) serveSSE(w http.ResponseWriter, requestContext context.Context, response *http.Response) accounting.Result {
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	result := accounting.Result{Outcome: "incomplete", IncompleteReason: accounting.ReasonStreamEnded}
	if response.StatusCode >= 400 {
		result.Outcome = "failed"
		result.IncompleteReason = accounting.ReasonNone
	}
	reader := bufio.NewReader(response.Body)
	var event []byte
	eventName := ""
	var dataLines []string
	terminal := false
	clientDisconnected := requestContext.Err() != nil
	var streamErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			if len(event) == 0 && strings.HasPrefix(text, ":") {
				if _, writeErr := w.Write(line); writeErr != nil {
					clientDisconnected = true
					streamErr = writeErr
					break
				}
				if canFlush {
					flusher.Flush()
				}
			} else {
				event = append(event, line...)
				// Only an empty line dispatches an event; spaces are SSE field content.
				if text == "" {
					if !terminal {
						if info, ok := parseSSEEvent(eventName, dataLines); ok {
							if isTerminalStatus(info.Status) {
								result = responseResult(info, response.StatusCode)
								terminal = true
							} else {
								result.ResponseModel = info.Model
								result.Status = info.Status
							}
						}
					}
					if _, writeErr := w.Write(event); writeErr != nil {
						clientDisconnected = true
						streamErr = writeErr
						break
					}
					if canFlush {
						flusher.Flush()
					}
					event = nil
					eventName = ""
					dataLines = nil
				} else {
					field, value, found := strings.Cut(text, ":")
					// SSE removes at most one optional space after the field separator.
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
			if len(event) > 0 {
				// Incomplete frames are forwarded but never update usage.
				if _, writeErr := w.Write(event); writeErr != nil {
					clientDisconnected = true
					streamErr = writeErr
				}
			}
			break
		}
	}
	if requestContext.Err() != nil {
		clientDisconnected = true
	}
	if !terminal && response.StatusCode < 400 {
		if clientDisconnected {
			result.IncompleteReason = accounting.ReasonClientDisconnected
		} else if streamErr != nil && !errors.Is(streamErr, io.EOF) {
			result.IncompleteReason = accounting.ReasonResponseReadError
		}
	}
	if streamErr != nil && !errors.Is(streamErr, io.EOF) && !clientDisconnected {
		s.logger.UpstreamError("upstream SSE stream ended early", "error", "stream read error")
	}
	return result
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
