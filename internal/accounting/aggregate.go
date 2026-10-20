package accounting

import (
	"encoding/json"
	"math/big"
)

// accumulator keeps aggregate sums exact even when they exceed SQLite's int64 range.
type accumulator struct {
	requests, input, output, total, cached, cacheWrite, reasoning big.Int
}

type groupKey struct {
	model string
	user  string
}

func newAccumulator() *accumulator { return &accumulator{} }

func (a *accumulator) add(usage Usage) {
	a.requests.Add(&a.requests, big.NewInt(1))
	a.input.Add(&a.input, big.NewInt(usage.InputTokens))
	a.output.Add(&a.output, big.NewInt(usage.OutputTokens))
	a.total.Add(&a.total, big.NewInt(usage.TotalTokens))
	a.cached.Add(&a.cached, big.NewInt(usage.CachedTokens))
	a.cacheWrite.Add(&a.cacheWrite, big.NewInt(usage.CacheWriteTokens))
	a.reasoning.Add(&a.reasoning, big.NewInt(usage.ReasoningTokens))
}

func (a *accumulator) summary() Summary {
	return Summary{
		RequestCount:     json.Number(a.requests.String()),
		InputTokens:      json.Number(a.input.String()),
		OutputTokens:     json.Number(a.output.String()),
		TotalTokens:      json.Number(a.total.String()),
		CachedTokens:     json.Number(a.cached.String()),
		CacheWriteTokens: json.Number(a.cacheWrite.String()),
		ReasoningTokens:  json.Number(a.reasoning.String()),
	}
}
