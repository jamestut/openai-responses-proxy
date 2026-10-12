package accounting

import (
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// RequestID identifies the single durable ledger row admitted for an operation.
type RequestID int64

// Kind identifies the create or compact operation admitted to the ledger.
type Kind string

const (
	KindCreate  Kind = "create"
	KindCompact Kind = "compact"
)

// Phase distinguishes observations that may still be refined from terminal results.
type Phase string

const (
	PhaseProvisional Phase = "provisional"
	PhaseFinal       Phase = "final"
)

// Usage contains only standard Responses token counters; detail counters are not
// added to TotalTokens because the upstream total already includes them.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// RequestMeta captures attribution before upstream work begins.
type RequestMeta struct {
	StartedAt time.Time
	User      string
	Model     string
	Kind      Kind
}

// Snapshot records one upstream observation. UsageKnown distinguishes an explicit
// zero-usage response from an observation that did not include usage.
type Snapshot struct {
	Phase         Phase
	ResponseID    string
	ResponseModel string
	Status        string
	Outcome       string
	HTTPStatus    int
	Usage         Usage
	UsageKnown    bool
}

// Dimension selects a public aggregate grouping key.
type Dimension string

const (
	GroupModel Dimension = "model"
	GroupUser  Dimension = "user"
)

// Filters stores exact matches and a half-open time range normalized to UTC.
type Filters struct {
	Model    string
	User     string
	GroupBy  []Dimension
	StartUTC *time.Time
	EndUTC   *time.Time
}

// Summary uses json.Number so large aggregate totals remain exact in JSON.
type Summary struct {
	RequestCount     json.Number `json:"request_count"`
	InputTokens      json.Number `json:"input_tokens"`
	OutputTokens     json.Number `json:"output_tokens"`
	TotalTokens      json.Number `json:"total_tokens"`
	CachedTokens     json.Number `json:"cached_tokens"`
	CacheWriteTokens json.Number `json:"cache_write_tokens"`
	ReasoningTokens  json.Number `json:"reasoning_tokens"`
	PendingCount     json.Number `json:"pending_count"`
	IncompleteCount  json.Number `json:"incomplete_count"`
}

// Group omits dimensions that were not requested by the caller.
type Group struct {
	Model *string `json:"model,omitempty"`
	User  *string `json:"user,omitempty"`
	Summary
}

// Report contains either ungrouped totals or grouped summaries.
type Report struct {
	Totals *Summary `json:"totals,omitempty"`
	Groups []Group  `json:"groups,omitempty"`
}

var (
	ErrUnavailable        = errors.New("accounting storage unavailable")
	ErrNotFound           = errors.New("accounting request not found")
	ErrResponseIDConflict = errors.New("response ID is already associated with another accounting request")
)

// ParseFilters strictly validates the query contract and converts calendar bounds
// in the requested IANA zone into a half-open UTC range.
func ParseFilters(values url.Values) (Filters, error) {
	allowed := map[string]bool{
		"model": true, "user": true, "group_by": true,
		"period": true, "start": true, "end": true, "timezone": true,
	}
	for key := range values {
		// Reject unknown and repeated keys so misspelled filters cannot broaden a query.
		if !allowed[key] {
			return Filters{}, queryError("unknown query parameter " + key)
		}
		if len(values[key]) != 1 {
			return Filters{}, queryError("query parameter must appear once: " + key)
		}
	}
	get := func(key string) string {
		if len(values[key]) == 0 {
			return ""
		}
		return values[key][0]
	}
	filters := Filters{Model: get("model"), User: get("user")}
	if has(values, "model") && filters.Model == "" {
		return Filters{}, queryError("model must not be empty")
	}
	if has(values, "user") && filters.User == "" {
		return Filters{}, queryError("user must not be empty")
	}
	if group := get("group_by"); group != "" {
		// Keep the accepted grouping combinations explicit to reject ambiguous spellings.
		switch group {
		case "model":
			filters.GroupBy = []Dimension{GroupModel}
		case "user":
			filters.GroupBy = []Dimension{GroupUser}
		case "model,user":
			filters.GroupBy = []Dimension{GroupModel, GroupUser}
		default:
			return Filters{}, queryError("group_by must be model, user, or model,user")
		}
	} else if has(values, "group_by") {
		return Filters{}, queryError("group_by must not be empty")
	}

	tzName := get("timezone")
	if has(values, "timezone") && tzName == "" {
		return Filters{}, queryError("timezone must not be empty")
	}
	if tzName == "" {
		// Stable UTC defaults keep identical queries independent of the host's local zone.
		tzName = "UTC"
	}
	if tzName == "Local" {
		return Filters{}, queryError("timezone must be an IANA timezone")
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return Filters{}, queryError("unknown timezone")
	}
	period := get("period")
	startText, endText := get("start"), get("end")
	if period != "" && (has(values, "start") || has(values, "end")) {
		return Filters{}, queryError("period cannot be combined with start or end")
	}
	if period != "" {
		// Convert both local calendar boundaries together so DST days keep their real duration.
		start, end, err := parsePeriod(period, loc)
		if err != nil {
			return Filters{}, err
		}
		start, end = start.UTC(), end.UTC()
		filters.StartUTC, filters.EndUTC = &start, &end
		return filters, nil
	}
	if has(values, "period") {
		return Filters{}, queryError("period must not be empty")
	}
	if has(values, "start") != has(values, "end") {
		return Filters{}, queryError("start and end must be supplied together")
	}
	if has(values, "start") {
		// Date-only bounds use the selected zone; timestamps must carry their own offset.
		start, err := parseBoundary(startText, loc)
		if err != nil {
			return Filters{}, err
		}
		end, err := parseBoundary(endText, loc)
		if err != nil {
			return Filters{}, err
		}
		if !end.After(start) {
			return Filters{}, queryError("end must be after start")
		}
		start, end = start.UTC(), end.UTC()
		filters.StartUTC, filters.EndUTC = &start, &end
	}
	return filters, nil
}

func has(values url.Values, key string) bool {
	_, ok := values[key]
	return ok
}

type queryError string

func (e queryError) Error() string { return string(e) }
