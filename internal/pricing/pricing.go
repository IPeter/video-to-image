// Package pricing turns model token usage into USD. Prices are copied from the product
// brief (docs/main-app.md) and must be rechecked before launch; the worker stores the
// computed cost at insert time so later price changes never rewrite history.
package pricing

import "strings"

// ReferenceDate is the date the table below was taken from the Gemini pricing page.
const ReferenceDate = "2026-09-23"

// Price is USD per one million tokens. Output pricing includes thinking tokens.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Note: the brief says gemini-3.8-flash rates apply through 2026-12-31 and rise from
// 2027-01-01. Update this table (and ReferenceDate) when that happens.
var table = map[string]Price{
	"gemini-3.5-flash-lite": {InputPerMTok: 0.30, OutputPerMTok: 2.50},
	"gemini-3.8-flash":      {InputPerMTok: 0.75, OutputPerMTok: 3.75},
	// Image model of the generative studio background (2026-09-24, Gemini API pricing
	// page): image output is billed per token at $60/M (a 1K image ≈ 1,120 tokens ≈
	// $0.067; ours are ~1,450 tokens); an input image is 1,120 tokens. The text
	// tokens of its short answer are billed at the same output rate here (a handful).
	"gemini-3.1-flash-image": {InputPerMTok: 0.30, OutputPerMTok: 60},
}

// Lookup finds the price for a model name. It accepts the "models/" prefix the API
// sometimes returns and versioned suffixes ("gemini-3.5-flash-lite-preview-10-2026")
// by longest known-prefix match.
func Lookup(model string) (Price, bool) {
	m := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	if p, ok := table[m]; ok {
		return p, true
	}
	best, bestLen := Price{}, 0
	for name, p := range table {
		if strings.HasPrefix(m, name+"-") && len(name) > bestLen {
			best, bestLen = p, len(name)
		}
	}
	return best, bestLen > 0
}

// Cost returns the USD cost of one call. known is false for models missing from the
// table, in which case cost is 0 and the caller should log a warning.
func Cost(model string, inputTokens, outputTokens, thinkingTokens int) (usd float64, known bool) {
	p, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	in := float64(inputTokens) * p.InputPerMTok / 1e6
	out := float64(outputTokens+thinkingTokens) * p.OutputPerMTok / 1e6
	return in + out, true
}
