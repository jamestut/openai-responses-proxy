package accounting

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounting.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store, path
}

func recordTestRow(t *testing.T, store *Store, startedAt time.Time, user, model string, usage Usage) {
	t.Helper()
	err := store.Record(context.Background(), RequestMeta{
		StartedAt: startedAt,
		User:      user,
		Model:     model,
		Kind:      KindCreate,
	}, Result{Usage: usage, Outcome: "completed", HTTPStatus: 200, Status: "completed"})
	if err != nil {
		t.Fatal(err)
	}
}

// Opening storage should create a private on-disk database with only the current accounting schema and indexes.
func TestOpenUsesPrivateOnDiskDatabaseAndSchema(t *testing.T) {
	store, path := openTestStore(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("database mode = %04o, want 0600", got)
	}

	for _, invalid := range []string{"", ":memory:", "file:test.db"} {
		t.Run("reject_"+strings.ReplaceAll(invalid, ":", "_"), func(t *testing.T) {
			if _, err := Open(invalid); err == nil {
				t.Fatalf("Open(%q) succeeded", invalid)
			}
		})
	}

	rows, err := store.db.Query(`PRAGMA table_info(accounting_requests)`)
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"started_at_us", "username", "model", "kind", "http_status", "response_status", "outcome", "input_tokens", "output_tokens", "total_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens", "incomplete_reason"} {
		if !columns[name] {
			t.Errorf("schema missing column %q", name)
		}
	}
	if len(columns) != 14 {
		t.Errorf("schema has %d columns, want exactly 14", len(columns))
	}
	for _, removed := range []string{"response_id", "pending", "usage_known"} {
		if columns[removed] {
			t.Errorf("schema has removed column %q", removed)
		}
	}

	var createSQL string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='accounting_requests'`).Scan(&createSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createSQL, "'nonterminal_response'") || !strings.Contains(createSQL, "'invalid_response'") {
		t.Fatalf("schema does not contain current incomplete_reason enum: %s", createSQL)
	}

	indexRows, err := store.db.Query(`PRAGMA index_list(accounting_requests)`)
	if err != nil {
		t.Fatal(err)
	}
	indexes := map[string]bool{}
	for indexRows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := indexRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		indexes[name] = true
	}
	if err := indexRows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"accounting_started_at_idx", "accounting_model_idx", "accounting_user_idx"} {
		if !indexes[name] {
			t.Errorf("schema missing index %q", name)
		}
	}
	if len(indexes) != 3 {
		t.Errorf("schema has %d indexes, want exactly 3", len(indexes))
	}
	for name := range indexes {
		if strings.Contains(name, "response") || strings.Contains(name, "pending") {
			t.Errorf("schema has removed-state index %q", name)
		}
	}
}

// A compact record must preserve attribution and the UTC instant, using the response model when needed.
// A missing user or unsupported operation kind must be rejected.
func TestRecordValidatesInputsAndStoresUTCAttribution(t *testing.T) {
	store, _ := openTestStore(t)
	started := time.Date(2026, 10, 4, 8, 12, 13, 123456000, time.FixedZone("offset", 7*60*60))
	if err := store.Record(context.Background(), RequestMeta{StartedAt: started, User: "alice", Kind: KindCompact}, Result{
		ResponseModel: "response-model", Outcome: "completed", HTTPStatus: 200, Status: "completed",
		Usage: Usage{InputTokens: 12, OutputTokens: 4, TotalTokens: 16, CachedTokens: 2, CacheWriteTokens: 1, ReasoningTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	var gotAt int64
	var user, model, kind, outcome, status string
	var httpStatus int
	var input, output, total, cached, cacheWrite, reasoning int64
	err := store.db.QueryRow(`SELECT started_at_us, username, model, kind, outcome, http_status, response_status,
input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens FROM accounting_requests`).Scan(
		&gotAt, &user, &model, &kind, &outcome, &httpStatus, &status,
		&input, &output, &total, &cached, &cacheWrite, &reasoning)
	if err != nil {
		t.Fatal(err)
	}
	if want := started.UTC().UnixMicro(); gotAt != want {
		t.Errorf("started_at_us = %d, want UTC instant %d", gotAt, want)
	}
	if user != "alice" || model != "response-model" || kind != "compact" || outcome != "completed" || status != "completed" || httpStatus != 200 {
		t.Errorf("stored metadata = %q %q %q %q %d %q", user, model, kind, outcome, httpStatus, status)
	}
	if got := []int64{input, output, total, cached, cacheWrite, reasoning}; !reflect.DeepEqual(got, []int64{12, 4, 16, 2, 1, 1}) {
		t.Errorf("stored usage = %v", got)
	}

	for name, meta := range map[string]RequestMeta{
		"missing_user": {Kind: KindCreate},
		"invalid_kind": {User: "alice", Kind: "retrieve"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Record(context.Background(), meta, Result{}); err == nil {
				t.Fatal("Record succeeded for invalid metadata")
			}
		})
	}
}

// SQLite should accept only the documented incomplete reasons and nonnegative counters for supported operation kinds.
func TestIncompleteReasonAndCounterConstraints(t *testing.T) {
	store, _ := openTestStore(t)
	allowed := []string{"", "transport_error", "response_read_error", "client_disconnected", "stream_ended", "nonterminal_response", "invalid_response"}
	for _, reason := range allowed {
		_, err := store.db.Exec(`INSERT INTO accounting_requests (
started_at_us, username, model, kind, http_status, response_status, outcome,
input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens, incomplete_reason
) VALUES (0, 'alice', '', 'create', 0, '', 'incomplete', 0, 0, 0, 0, 0, 0, ?)`, reason)
		if err != nil {
			t.Errorf("allowed reason %q rejected: %v", reason, err)
		}
	}
	for name, statement := range map[string]string{
		"arbitrary_reason": `INSERT INTO accounting_requests (started_at_us, username, kind, http_status, outcome, input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens, incomplete_reason) VALUES (0, 'alice', 'create', 0, 'incomplete', 0, 0, 0, 0, 0, 0, 'arbitrary_reason')`,
		"invalid_kind":     `INSERT INTO accounting_requests (started_at_us, username, kind, http_status, outcome, input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens, incomplete_reason) VALUES (0, 'alice', 'retrieve', 0, 'incomplete', 0, 0, 0, 0, 0, 0, '')`,
		"negative_counter": `INSERT INTO accounting_requests (started_at_us, username, kind, http_status, outcome, input_tokens, output_tokens, total_tokens, cached_tokens, cache_write_tokens, reasoning_tokens, incomplete_reason) VALUES (0, 'alice', 'create', 0, 'incomplete', -1, 0, 0, 0, 0, 0, '')`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.db.Exec(statement); err == nil {
				t.Fatal("invalid row was accepted")
			}
		})
	}
}

// Queries should group and filter exact usage, return empty results consistently, and leave stored rows untouched.
func TestQueryGroupingFiltersEmptyResultsAndExactLargeTotals(t *testing.T) {
	store, _ := openTestStore(t)
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two individually valid maximum counters make aggregate totals exceed SQLite's signed integer range.
	max := int64(math.MaxInt64)
	rows := []struct {
		user, model string
		usage       Usage
	}{
		{"alice", "gpt-6-luna", Usage{InputTokens: max, OutputTokens: 2, TotalTokens: max, CachedTokens: 3}},
		{"bob", "gpt-6-luna", Usage{InputTokens: max, OutputTokens: 4, TotalTokens: max, CacheWriteTokens: 5}},
		{"alice", "other-model", Usage{InputTokens: 7, OutputTokens: 6, TotalTokens: 13, ReasoningTokens: 8}},
	}
	for i, row := range rows {
		recordTestRow(t, store, when.Add(time.Duration(i)*time.Second), row.user, row.model, row.usage)
	}

	tests := []struct {
		name   string
		filter Filters
		check  func(*testing.T, Report)
	}{
		{"ungrouped_exact_bigint", Filters{}, func(t *testing.T, report Report) {
			if report.Totals == nil || len(report.Groups) != 0 {
				t.Fatalf("unexpected report shape: %+v", report)
			}
			// Keep expected sums literal so the test independently checks exact aggregation.
			want := Summary{RequestCount: "3", InputTokens: "18446744073709551621", OutputTokens: "12", TotalTokens: "18446744073709551627", CachedTokens: "3", CacheWriteTokens: "5", ReasoningTokens: "8"}
			if *report.Totals != want {
				t.Errorf("totals = %+v, want %+v", *report.Totals, want)
			}
		}},
		{"group_by_model", Filters{GroupBy: []Dimension{GroupModel}}, func(t *testing.T, report Report) {
			if len(report.Groups) != 2 || report.Groups[0].Model == nil || *report.Groups[0].Model != "gpt-6-luna" || report.Groups[0].User != nil || report.Groups[0].RequestCount != "2" || report.Groups[1].Model == nil || *report.Groups[1].Model != "other-model" {
				t.Fatalf("unexpected model groups: %+v", report.Groups)
			}
		}},
		{"group_by_user", Filters{GroupBy: []Dimension{GroupUser}}, func(t *testing.T, report Report) {
			if len(report.Groups) != 2 || report.Groups[0].User == nil || *report.Groups[0].User != "alice" || report.Groups[0].Model != nil || report.Groups[0].RequestCount != "2" || report.Groups[1].User == nil || *report.Groups[1].User != "bob" {
				t.Fatalf("unexpected user groups: %+v", report.Groups)
			}
		}},
		{"group_by_both", Filters{GroupBy: []Dimension{GroupModel, GroupUser}}, func(t *testing.T, report Report) {
			if len(report.Groups) != 3 || report.Groups[0].Model == nil || *report.Groups[0].Model != "gpt-6-luna" || report.Groups[0].User == nil || *report.Groups[0].User != "alice" || report.Groups[1].User == nil || *report.Groups[1].User != "bob" {
				t.Fatalf("unexpected combined groups: %+v", report.Groups)
			}
		}},
		{"exact_model_user", Filters{Model: "gpt-6-luna", User: "bob"}, func(t *testing.T, report Report) {
			if report.Totals == nil || report.Totals.RequestCount != "1" || report.Totals.InputTokens != "9223372036854775807" {
				t.Fatalf("unexpected filtered totals: %+v", report.Totals)
			}
		}},
		{"unknown_ungrouped", Filters{Model: "missing"}, func(t *testing.T, report Report) {
			if report.Totals == nil || report.Totals.RequestCount != "0" || report.Totals.TotalTokens != "0" {
				t.Fatalf("unexpected empty totals: %+v", report.Totals)
			}
		}},
		{"unknown_grouped", Filters{User: "missing", GroupBy: []Dimension{GroupUser}}, func(t *testing.T, report Report) {
			if report.Groups == nil || len(report.Groups) != 0 || report.Totals != nil {
				t.Fatalf("unexpected empty groups: %+v", report)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := store.Query(context.Background(), test.filter)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, report)
		})
	}

	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM accounting_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(rows) {
		t.Fatalf("queries changed row count to %d", count)
	}
}

// Filter parsing should accept supported grouping syntax and reject ambiguous, malformed, or incomplete query parameters.
func TestParseFiltersStrictContract(t *testing.T) {
	valid := []struct {
		name   string
		query  string
		groups []Dimension
	}{
		{"none", "", nil},
		{"model", "group_by=model", []Dimension{GroupModel}},
		{"user", "group_by=user", []Dimension{GroupUser}},
		{"both", "group_by=model%2Cuser", []Dimension{GroupModel, GroupUser}},
	}
	for _, test := range valid {
		t.Run("valid_"+test.name, func(t *testing.T) {
			filters, err := ParseFilters(mustValues(t, test.query))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(filters.GroupBy, test.groups) {
				t.Errorf("groups = %v, want %v", filters.GroupBy, test.groups)
			}
		})
	}

	invalid := []struct {
		name  string
		query string
	}{
		{"unknown_parameter", "modle=x"},
		{"repeated_parameter", "model=a&model=b"},
		{"empty_model", "model="},
		{"empty_user", "user="},
		{"empty_grouping", "group_by="},
		{"invalid_grouping", "group_by=model%2Cuser%2Cother"},
		{"empty_timezone", "timezone="},
		{"local_timezone", "timezone=Local"},
		{"invalid_timezone", "timezone=Not%2FAZone"},
		{"empty_period", "period="},
		{"malformed_period", "period=2026-2"},
		{"year_zero", "period=0000"},
		{"invalid_month", "period=2026-13"},
		{"invalid_date", "period=2026-02-30"},
		{"period_with_start", "period=2026&start=2026-01-01"},
		{"only_start", "start=2026-01-01"},
		{"only_end", "end=2026-01-02"},
		{"datetime_without_offset", "start=2026-01-01T00%3A00%3A00&end=2026-01-02T00%3A00%3A00Z"},
		{"end_not_after_start", "start=2026-01-02T00%3A00%3A00Z&end=2026-01-02T00%3A00%3A00Z"},
		{"repeated_grouping", "group_by=model&group_by=user"},
	}
	for _, test := range invalid {
		t.Run("invalid_"+test.name, func(t *testing.T) {
			if _, err := ParseFilters(mustValues(t, test.query)); err == nil {
				t.Fatalf("ParseFilters(%q) succeeded", test.query)
			}
		})
	}
}

func mustValues(t *testing.T, query string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// Sydney calendar days should retain their 23- or 25-hour DST length and query the half-open interval [start, end).
func TestParseFiltersSydneyDSTAndHalfOpenBoundaries(t *testing.T) {
	tests := []struct {
		name, period, start, end string
		duration                 time.Duration
	}{
		{"spring_forward", "2026-10-04", "2026-10-03T14:00:00Z", "2026-10-04T13:00:00Z", 23 * time.Hour},
		{"fall_back", "2026-04-05", "2026-04-04T13:00:00Z", "2026-04-05T14:00:00Z", 25 * time.Hour},
	}
	// These UTC bounds and durations are fixed expectations for Sydney's 2026 clock changes.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filters, err := ParseFilters(mustValues(t, "period="+test.period+"&timezone=Australia%2FSydney"))
			if err != nil {
				t.Fatal(err)
			}
			if filters.StartUTC == nil || filters.EndUTC == nil {
				t.Fatal("period did not produce time bounds")
			}
			start, _ := time.Parse(time.RFC3339, test.start)
			end, _ := time.Parse(time.RFC3339, test.end)
			if !filters.StartUTC.Equal(start) || !filters.EndUTC.Equal(end) || filters.EndUTC.Sub(*filters.StartUTC) != test.duration {
				t.Fatalf("period bounds = [%s, %s), want [%s, %s) (%s)", filters.StartUTC, filters.EndUTC, start, end, test.duration)
			}

			store, _ := openTestStore(t)
			recordTestRow(t, store, start.Add(-time.Second), "alice", "before", Usage{TotalTokens: 1})
			recordTestRow(t, store, start, "alice", "at-start", Usage{TotalTokens: 2})
			recordTestRow(t, store, end.Add(-time.Second), "bob", "inside", Usage{TotalTokens: 3})
			recordTestRow(t, store, end, "bob", "at-end", Usage{TotalTokens: 4})
			report, err := store.Query(context.Background(), filters)
			if err != nil {
				t.Fatal(err)
			}
			if report.Totals == nil || report.Totals.RequestCount != "2" || report.Totals.TotalTokens != "5" {
				t.Fatalf("period selected unexpected rows: %+v", report.Totals)
			}

			rangeFilters, err := ParseFilters(mustValues(t, "start="+url.QueryEscape(test.start)+"&end="+url.QueryEscape(test.end)))
			if err != nil {
				t.Fatal(err)
			}
			if rangeFilters.StartUTC == nil || rangeFilters.EndUTC == nil {
				t.Fatal("explicit range did not produce time bounds")
			}
			if !rangeFilters.StartUTC.Equal(start) || !rangeFilters.EndUTC.Equal(end) {
				t.Fatalf("equivalent range bounds = [%s, %s)", rangeFilters.StartUTC, rangeFilters.EndUTC)
			}
			periodFilters := filters
			periodFilters.GroupBy = []Dimension{GroupModel}
			rangeFilters.GroupBy = []Dimension{GroupModel}
			periodReport, err := store.Query(context.Background(), periodFilters)
			if err != nil {
				t.Fatal(err)
			}
			rangeReport, err := store.Query(context.Background(), rangeFilters)
			if err != nil {
				t.Fatal(err)
			}
			if len(periodReport.Groups) != 2 || len(rangeReport.Groups) != 2 {
				t.Fatalf("period/range group counts = %d/%d, want 2/2", len(periodReport.Groups), len(rangeReport.Groups))
			}
			for i, wantModel := range []string{"at-start", "inside"} {
				if periodReport.Groups[i].Model == nil || *periodReport.Groups[i].Model != wantModel || rangeReport.Groups[i].Model == nil || *rangeReport.Groups[i].Model != wantModel {
					t.Errorf("period/range group %d did not select model %q", i, wantModel)
				}
			}
		})
	}
}

// Calendar filters should include only instants inside each UTC period, with UTC used when no zone is specified.
func TestParseFiltersCalendarPeriodsAndUTCDefault(t *testing.T) {
	store, _ := openTestStore(t)
	instants := []string{
		"2025-12-31T23:59:59Z", "2026-01-01T00:00:00Z", "2026-09-30T23:59:59Z",
		"2026-10-01T00:00:00Z", "2026-10-31T23:59:59Z", "2026-11-01T00:00:00Z", "2027-01-01T00:00:00Z",
	}
	// Rows immediately around year and month edges make inclusion and exclusion explicit.
	for i, instant := range instants {
		started, err := time.Parse(time.RFC3339, instant)
		if err != nil {
			t.Fatal(err)
		}
		recordTestRow(t, store, started, "alice", "fixture", Usage{TotalTokens: int64(i + 1)})
	}
	for _, test := range []struct {
		query    string
		requests string
		tokens   string
	}{
		{"period=2026", "5", "20"},
		{"period=2026-10", "2", "9"},
		{"period=2026-10-04", "0", "0"},
	} {
		// Literal counts and token sums are derived from the fixture instants above.
		filters, err := ParseFilters(mustValues(t, test.query))
		if err != nil {
			t.Fatalf("ParseFilters(%q): %v", test.query, err)
		}
		report, err := store.Query(context.Background(), filters)
		if err != nil {
			t.Fatal(err)
		}
		if report.Totals == nil || string(report.Totals.RequestCount) != test.requests || string(report.Totals.TotalTokens) != test.tokens {
			t.Errorf("query %q selected %+v, want requests=%s total_tokens=%s", test.query, report.Totals, test.requests, test.tokens)
		}
	}
	implicit, err := ParseFilters(mustValues(t, "period=2026-10-04"))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := ParseFilters(mustValues(t, "period=2026-10-04&timezone=UTC"))
	if err != nil {
		t.Fatal(err)
	}
	if implicit.StartUTC == nil || implicit.EndUTC == nil || explicit.StartUTC == nil || explicit.EndUTC == nil {
		t.Fatal("calendar period did not produce time bounds")
	}
	if !implicit.StartUTC.Equal(*explicit.StartUTC) || !implicit.EndUTC.Equal(*explicit.EndUTC) {
		t.Fatalf("implicit UTC bounds differ: [%s,%s) vs [%s,%s)", implicit.StartUTC, implicit.EndUTC, explicit.StartUTC, explicit.EndUTC)
	}
}

// Usage parsing should read standard Responses counters and leave missing or malformed counters at zero.
func TestParseUsageObjectStandardAndMalformedCounters(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Usage
	}{
		{"standard", `{"input_tokens":12,"output_tokens":4,"total_tokens":16,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1},"output_tokens_details":{"reasoning_tokens":1}}`, Usage{12, 4, 16, 2, 1, 1}},
		{"empty", ``, Usage{}},
		{"null", `null`, Usage{}},
		{"malformed_json", `{`, Usage{}},
		{"non_object", `[]`, Usage{}},
		{"bad_fields", `{"input_tokens":-1,"output_tokens":1.5,"total_tokens":"8","input_tokens_details":[],"output_tokens_details":{"reasoning_tokens":9223372036854775808}}`, Usage{}},
		{"partial_fields", `{"input_tokens":5,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":-3},"output_tokens_details":{"reasoning_tokens":"4"}}`, Usage{InputTokens: 5, CachedTokens: 2}},
		{"maximum_integer", `{"total_tokens":9223372036854775807}`, Usage{TotalTokens: math.MaxInt64}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseUsageObject(json.RawMessage(test.raw))
			if got != test.want {
				t.Errorf("ParseUsageObject(%s) = %+v, want %+v", test.raw, got, test.want)
			}
		})
	}
}
