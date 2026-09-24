package config

import (
	"testing"
	"time"

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

func TestStudioSettings(t *testing.T) {
	c, err := Load()
	require.NoError(t, err)
	assert.Empty(t, c.StudioURL, "studio backgrounds are off unless a sidecar is configured")
	assert.False(t, c.StudioEnabled())
	assert.Equal(t, 45*time.Second, c.StudioTimeout)
	assert.Equal(t, 1, c.StudioWorkers)
	require.NoError(t, c.RequireStudio())

	t.Setenv("V2I_STUDIO_URL", "http://127.0.0.1:4100 ")
	t.Setenv("V2I_STUDIO_TIMEOUT", "20s")
	t.Setenv("V2I_STUDIO_WORKERS", "2")
	c, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:4100", c.StudioURL)
	assert.True(t, c.StudioEnabled())
	assert.Equal(t, 20*time.Second, c.StudioTimeout)
	assert.Equal(t, 2, c.StudioWorkers)
	require.NoError(t, c.RequireStudio())

	for _, tc := range []struct {
		name string
		c    Config
		ok   bool
	}{
		{"off", Config{StudioTimeout: time.Second, StudioWorkers: 1}, true},
		{"fake", Config{StudioURL: StudioFake, StudioTimeout: time.Second, StudioWorkers: 1}, true},
		{"https", Config{StudioURL: "https://studio.internal", StudioTimeout: time.Second, StudioWorkers: 1}, true},
		{"no scheme", Config{StudioURL: "127.0.0.1:4100", StudioTimeout: time.Second, StudioWorkers: 1}, false},
		{"other scheme", Config{StudioURL: "ftp://x", StudioTimeout: time.Second, StudioWorkers: 1}, false},
		{"no timeout", Config{StudioURL: StudioFake, StudioWorkers: 1}, false},
		{"no workers", Config{StudioURL: StudioFake, StudioTimeout: time.Second}, false},
	} {
		err := tc.c.RequireStudio()
		if tc.ok {
			assert.NoError(t, err, tc.name)
		} else {
			assert.Error(t, err, tc.name)
		}
	}

	// The fake segmenter is refused with a paid analyzer.
	assert.NoError(t, (&Config{Analyzer: AnalyzerFake, StudioURL: StudioFake}).RequireSafeDefaults())
	assert.ErrorContains(t, (&Config{Analyzer: AnalyzerGemini, StudioURL: StudioFake}).RequireSafeDefaults(), "V2I_STUDIO_URL=fake")
	assert.NoError(t, (&Config{Analyzer: AnalyzerGemini, StudioURL: "http://127.0.0.1:4100"}).RequireSafeDefaults())
}
