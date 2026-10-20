package proxy

import (
	"encoding/json"
	"strings"

	"openai-responses-proxy/internal/accounting"
)

type responseInfo struct {
	Model   string
	Status  string
	Usage   accounting.Usage
	Compact bool
	Valid   bool
}

// parseResponse accepts either a Response object or an event envelope containing response.
func parseResponse(body []byte) responseInfo {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return responseInfo{}
	}
	responseRaw := envelope["response"]
	if len(responseRaw) == 0 {
		responseRaw = body
	}
	var object struct {
		Model  string          `json:"model"`
		Status string          `json:"status"`
		Object string          `json:"object"`
		Usage  json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(responseRaw, &object); err != nil {
		return responseInfo{}
	}
	if len(object.Usage) == 0 {
		object.Usage = envelope["usage"]
	}
	usage := accounting.ParseUsageObject(object.Usage)
	return responseInfo{
		Model:   object.Model,
		Status:  strings.ToLower(object.Status),
		Usage:   usage,
		Compact: object.Object == "response.compaction",
		Valid:   object.Status != "" || object.Object == "response" || object.Object == "response.compaction",
	}
}

func isTerminalStatus(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "failed", "incomplete", "cancelled", "canceled":
		return true
	default:
		return false
	}
}
