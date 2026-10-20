package accounting

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// ParseUsageObject reads one Responses usage object without decoding its envelope.
func ParseUsageObject(usageRaw json.RawMessage) Usage {
	if len(usageRaw) == 0 || bytes.Equal(bytes.TrimSpace(usageRaw), []byte("null")) {
		return Usage{}
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(usageRaw, &values); err != nil || values == nil {
		return Usage{}
	}
	usage := Usage{
		// Detail counters are reported separately and must not be folded into total tokens.
		InputTokens:      nonnegativeInt(values["input_tokens"]),
		OutputTokens:     nonnegativeInt(values["output_tokens"]),
		TotalTokens:      nonnegativeInt(values["total_tokens"]),
		CachedTokens:     nestedNonnegativeInt(values, "input_tokens_details", "cached_tokens"),
		CacheWriteTokens: nestedNonnegativeInt(values, "input_tokens_details", "cache_write_tokens"),
		ReasoningTokens:  nestedNonnegativeInt(values, "output_tokens_details", "reasoning_tokens"),
	}
	return usage
}

func nestedNonnegativeInt(values map[string]json.RawMessage, object, field string) int64 {
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(values[object], &nested); err != nil {
		return 0
	}
	return nonnegativeInt(nested[field])
}

func nonnegativeInt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	text := string(bytes.TrimSpace(raw))
	// Only nonnegative JSON integer literals count; malformed counters stay zero.
	if !json.Valid([]byte(text)) || text == "" || text[0] == '"' || text[0] == '-' {
		return 0
	}
	// ParseInt avoids float64 rounding and rejects values outside the stored int64 range.
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
