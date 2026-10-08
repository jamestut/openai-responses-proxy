package accounting

import (
	"encoding/json"
	"math/big"
)

// accumulator keeps sums wider than SQLite's per-row int64 counters so long-lived
// totals do not overflow before they are serialized.
type accumulator struct {
	requests   big.Int
	input      big.Int
	output     big.Int
	total      big.Int
	cached     big.Int
	cacheWrite big.Int
	reasoning  big.Int
	pending    big.Int
	incomplete big.Int
}

type groupKey struct {
	model string
	user  string
}

func newAccumulator() *accumulator { return &accumulator{} }

func (a *accumulator) add(state string, usage Usage) {
	// Every matched ledger row counts once, including rows with missing usage.
	a.requests.Add(&a.requests, big.NewInt(1))
	a.input.Add(&a.input, big.NewInt(usage.InputTokens))
	a.output.Add(&a.output, big.NewInt(usage.OutputTokens))
	a.total.Add(&a.total, big.NewInt(usage.TotalTokens))
	a.cached.Add(&a.cached, big.NewInt(usage.CachedTokens))
	a.cacheWrite.Add(&a.cacheWrite, big.NewInt(usage.CacheWriteTokens))
	a.reasoning.Add(&a.reasoning, big.NewInt(usage.ReasoningTokens))
	switch state {
	case usagePending:
		a.pending.Add(&a.pending, big.NewInt(1))
	case usageIncomplete:
		a.incomplete.Add(&a.incomplete, big.NewInt(1))
	}
}

func (a *accumulator) summary() Summary {
	// String-backed JSON numbers preserve the full big.Int value without quoting it.
	return Summary{
		RequestCount:     json.Number(a.requests.String()),
		InputTokens:      json.Number(a.input.String()),
		OutputTokens:     json.Number(a.output.String()),
		TotalTokens:      json.Number(a.total.String()),
		CachedTokens:     json.Number(a.cached.String()),
		CacheWriteTokens: json.Number(a.cacheWrite.String()),
		ReasoningTokens:  json.Number(a.reasoning.String()),
		PendingCount:     json.Number(a.pending.String()),
		IncompleteCount:  json.Number(a.incomplete.String()),
	}
}
