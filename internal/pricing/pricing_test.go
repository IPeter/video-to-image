package pricing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCostMatchesBriefEstimate(t *testing.T) {
	// Brief: 120 s video at ~100-300 tok/s → 12k-36k input tokens, ~1k output tokens,
	// ≈ $0.006-$0.014 per analysis on Flash-Lite.
	lo, ok := Cost("gemini-3.5-flash-lite", 12_000, 1_000, 0)
	assert.True(t, ok)
	assert.InDelta(t, 0.0061, lo, 0.0001)
	hi, _ := Cost("gemini-3.5-flash-lite", 36_000, 1_000, 0)
	assert.InDelta(t, 0.0133, hi, 0.0001)
}

func TestThinkingBilledAsOutput(t *testing.T) {
	a, _ := Cost("gemini-3.8-flash", 0, 1000, 0)
	b, _ := Cost("gemini-3.8-flash", 0, 500, 500)
	assert.InDelta(t, a, b, 1e-12)
}

func TestLookupVariants(t *testing.T) {
	_, ok := Lookup("models/gemini-3.5-flash-lite")
	assert.True(t, ok)
	p, ok := Lookup("gemini-3.5-flash-lite-preview-10-2026")
	assert.True(t, ok)
	assert.Equal(t, 0.30, p.InputPerMTok)
	_, ok = Lookup("gemini-3.5-flash-lite2")
	assert.False(t, ok)
	c, ok := Cost("unknown-model", 1000, 1000, 0)
	assert.False(t, ok)
	assert.Zero(t, c)
}
