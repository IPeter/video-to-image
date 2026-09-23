package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GNU make's `include .env` keeps the spaces before an inline comment; Load must not
// fail on them.
func TestLoadTrimsWhitespace(t *testing.T) {
	t.Setenv("DAILY_COST_CEILING_USD", "7     ")
	t.Setenv("V2I_LOG_FORMAT", "text   ")
	t.Setenv("V2I_TRUSTED_PROXIES", " 10.42.0.0/16,10.43.0.1 ")
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 7.0, c.DailyCostCeilingUSD)
	assert.Equal(t, "text", c.LogFormat)
	assert.Equal(t, []string{"10.42.0.0/16", "10.43.0.1"}, c.TrustedProxies)
}

func TestDefaultsAreFailSafe(t *testing.T) {
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 5.0, c.DailyCostCeilingUSD, "cost guard is on unless disabled on purpose")
	assert.Empty(t, c.IPHashSalt, "no public default salt")
	assert.Equal(t, ":4001", c.AdminAddr)
}

func TestRequireSafeDefaults(t *testing.T) {
	cases := []struct {
		analyzer, token string
		ok              bool
	}{
		{AnalyzerFake, "dev-internal-token", true},
		{AnalyzerRecorded, "short", true},
		{AnalyzerGemini, "", true},
		{AnalyzerGemini, "dev-internal-token", false},
		{AnalyzerGemini, "too-short", false},
		{AnalyzerRecording, "0123456789abcdef0123456789abcdef", true},
	}
	for _, tc := range cases {
		c := &Config{Analyzer: tc.analyzer, InternalToken: tc.token}
		err := c.RequireSafeDefaults()
		if tc.ok {
			assert.NoError(t, err, "%s/%q", tc.analyzer, tc.token)
		} else {
			assert.Error(t, err, "%s/%q", tc.analyzer, tc.token)
		}
	}
}
