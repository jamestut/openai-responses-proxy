package proxy

import (
	"encoding/json"
	"strings"

	"openai-responses-proxy/internal/accounting"
)

type responseInfo struct {
	ID         string
	Model      string
	Status     string
	Usage      accounting.Usage
	UsageKnown bool
	Valid      bool
}

// parseResponse accepts both a bare Response object and an event envelope containing response.
func parseResponse(body []byte) responseInfo {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return responseInfo{}
	}
	responseRaw := envelope["response"]
	if len(responseRaw) == 0 {
		// Non-streaming retrieve/create responses are themselves the Response object.
		responseRaw = body
	}
	var object struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(responseRaw, &object); err != nil {
		return responseInfo{}
	}
	// Usage parsing receives the original envelope so it can handle either response shape.
	usage, known := accounting.ParseUsage(body)
	return responseInfo{
		ID: object.ID, Model: object.Model, Status: strings.ToLower(object.Status),
		Usage: usage, UsageKnown: known,
		Valid: object.ID != "" || object.Status != "" || object.Object == "response",
	}
}

func isTerminalStatus(status string) bool {
	// Accept both spellings because upstreams may report canceled or cancelled.
	switch strings.ToLower(status) {
	case "completed", "failed", "incomplete", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func isPendingStatus(status string) bool {
	switch strings.ToLower(status) {
	case "in_progress", "queued":
		return true
	default:
		return false
	}
}

func (info responseInfo) snapshot(phase accounting.Phase, httpStatus int, outcome string) accounting.Snapshot {
	// Keep Responses parsing separate from the accounting store's durable snapshot format.
	return accounting.Snapshot{
		Phase: phase, ResponseID: info.ID, ResponseModel: info.Model, Status: info.Status,
		Outcome: outcome, HTTPStatus: httpStatus, Usage: info.Usage, UsageKnown: info.UsageKnown,
	}
}
