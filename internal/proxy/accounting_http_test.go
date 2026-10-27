package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"openai-responses-proxy/internal/accounting"
)

// Seed three operations across two users and models. Both users must see the same
// global summaries, with only the requested groups and exact filter matches.
func TestAccountingHTTPGlobalGroupsAndExactFilters(t *testing.T) {
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("accounting query unexpectedly reached upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}), nil)
	for _, row := range []struct {
		user, model string
		usage       accounting.Usage
	}{
		{"alice", "model-a", accounting.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10, CachedTokens: 1, CacheWriteTokens: 2, ReasoningTokens: 1}},
		{"bob", "model-a", accounting.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, CachedTokens: 1, ReasoningTokens: 2}},
		{"alice", "model-b", accounting.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7, ReasoningTokens: 1}},
	} {
		if err := h.store.Record(context.Background(), accounting.RequestMeta{
			StartedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), User: row.user, Model: row.model,
		}, accounting.Result{HTTPStatus: 200, Status: "completed", Outcome: "completed", Usage: row.usage}); err != nil {
			t.Fatal(err)
		}
	}

	// Spell out the expected totals and fields so the test does not reuse the
	// aggregation logic it is checking. Extra fields must also fail the comparison.
	cases := []struct{ name, query, want string }{
		{"global_totals", "", `{
  "totals": {
    "request_count": 3,
    "input_tokens": 9,
    "output_tokens": 13,
    "total_tokens": 22,
    "cached_tokens": 2,
    "cache_write_tokens": 2,
    "reasoning_tokens": 4
  }
}`},
		{"by_model", "?group_by=model", `{
  "groups": [
    {
      "model": "model-a",
      "request_count": 2,
      "input_tokens": 6,
      "output_tokens": 9,
      "total_tokens": 15,
      "cached_tokens": 2,
      "cache_write_tokens": 2,
      "reasoning_tokens": 3
    },
    {
      "model": "model-b",
      "request_count": 1,
      "input_tokens": 3,
      "output_tokens": 4,
      "total_tokens": 7,
      "cached_tokens": 0,
      "cache_write_tokens": 0,
      "reasoning_tokens": 1
    }
  ]
}`},
		{"by_user", "?group_by=user", `{
  "groups": [
    {
      "user": "alice",
      "request_count": 2,
      "input_tokens": 7,
      "output_tokens": 10,
      "total_tokens": 17,
      "cached_tokens": 1,
      "cache_write_tokens": 2,
      "reasoning_tokens": 2
    },
    {
      "user": "bob",
      "request_count": 1,
      "input_tokens": 2,
      "output_tokens": 3,
      "total_tokens": 5,
      "cached_tokens": 1,
      "cache_write_tokens": 0,
      "reasoning_tokens": 2
    }
  ]
}`},
		{"by_model_and_user", "?group_by=model,user", `{
  "groups": [
    {
      "model": "model-a",
      "user": "alice",
      "request_count": 1,
      "input_tokens": 4,
      "output_tokens": 6,
      "total_tokens": 10,
      "cached_tokens": 1,
      "cache_write_tokens": 2,
      "reasoning_tokens": 1
    },
    {
      "model": "model-a",
      "user": "bob",
      "request_count": 1,
      "input_tokens": 2,
      "output_tokens": 3,
      "total_tokens": 5,
      "cached_tokens": 1,
      "cache_write_tokens": 0,
      "reasoning_tokens": 2
    },
    {
      "model": "model-b",
      "user": "alice",
      "request_count": 1,
      "input_tokens": 3,
      "output_tokens": 4,
      "total_tokens": 7,
      "cached_tokens": 0,
      "cache_write_tokens": 0,
      "reasoning_tokens": 1
    }
  ]
}`},
		{"exact_model", "?model=model-a", `{
  "totals": {
    "request_count": 2,
    "input_tokens": 6,
    "output_tokens": 9,
    "total_tokens": 15,
    "cached_tokens": 2,
    "cache_write_tokens": 2,
    "reasoning_tokens": 3
  }
}`},
		{"exact_user", "?user=alice", `{
  "totals": {
    "request_count": 2,
    "input_tokens": 7,
    "output_tokens": 10,
    "total_tokens": 17,
    "cached_tokens": 1,
    "cache_write_tokens": 2,
    "reasoning_tokens": 2
  }
}`},
		{"combined_filters", "?model=model-a&user=bob", `{
  "totals": {
    "request_count": 1,
    "input_tokens": 2,
    "output_tokens": 3,
    "total_tokens": 5,
    "cached_tokens": 1,
    "cache_write_tokens": 0,
    "reasoning_tokens": 2
  }
}`},
		{"no_prefix_matches", "?model=model", `{
  "totals": {
    "request_count": 0,
    "input_tokens": 0,
    "output_tokens": 0,
    "total_tokens": 0,
    "cached_tokens": 0,
    "cache_write_tokens": 0,
    "reasoning_tokens": 0
  }
}`},
		{"empty_grouped", "?user=unknown&group_by=user", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Authentication grants access to global data, not just the caller's rows.
			for _, key := range []string{"alice-test-key", "bob-test-key"} {
				response := h.do(t, http.MethodGet, "/accounting"+tc.query, key, "", nil)
				if response.StatusCode != http.StatusOK {
					response.Body.Close()
					t.Fatalf("accounting HTTP status = %d, want 200", response.StatusCode)
				}
				if got := response.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				var got, want any
				// Keep JSON numbers exact instead of converting them to floating point.
				decoder := json.NewDecoder(response.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&got); err != nil {
					response.Body.Close()
					t.Fatal(err)
				}
				response.Body.Close()
				wantDecoder := json.NewDecoder(strings.NewReader(tc.want))
				wantDecoder.UseNumber()
				if err := wantDecoder.Decode(&want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("summary = %v; want %v", got, want)
				}
			}
		})
	}
	// Reading summaries must leave the three original operations untouched.
	if got := len(records(t, h.dbPath)); got != 3 {
		t.Errorf("row count after accounting reads = %d, want 3", got)
	}
}

// Invalid filters must return 400; POST must return 405 and advertise GET.
// None of these requests may reach upstream or create accounting records.
func TestAccountingHTTPRejectsInvalidQueriesAndMethods(t *testing.T) {
	h := newHarnessWithUpstream(t, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("accounting request unexpectedly reached upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}), nil)
	for _, query := range []string{
		"period=2026-13", "timezone=Invalid/Zone", "timezone=Local", "start=2026-01-01", "end=2026-01-02",
		"start=2026-01-01T00:00:00&end=2026-01-02T00:00:00", "start=2026-01-02&end=2026-01-01",
		"period=2026&start=2026-01-01&end=2026-02-01", "unknown=value", "user=alice&user=bob",
		"group_by=user,model", "group_by=invalid", "group_by=", "model=", "user=", "period=", "timezone=",
	} {
		t.Run(query, func(t *testing.T) {
			response := h.do(t, http.MethodGet, "/accounting?"+query, "alice-test-key", "", nil)
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("HTTP status = %d, want 400", response.StatusCode)
			}
		})
	}
	response := h.do(t, http.MethodPost, "/accounting", "alice-test-key", "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Errorf("POST /accounting status = %d, Allow = %q; want 405, GET", response.StatusCode, response.Header.Get("Allow"))
	}
	if got := len(records(t, h.dbPath)); got != 0 {
		t.Errorf("row count after rejected accounting requests = %d, want 0", got)
	}
}
