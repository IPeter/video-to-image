package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/config"
	"video-to-image/internal/studio"
)

func TestNewSegmenter(t *testing.T) {
	seg, err := NewSegmenter(&config.Config{})
	require.NoError(t, err)
	assert.Nil(t, seg, "no V2I_STUDIO_URL: studio backgrounds off")

	seg, err = NewSegmenter(&config.Config{StudioURL: config.StudioFake})
	require.NoError(t, err)
	assert.Equal(t, studio.FakeSegmenter{}, seg)

	seg, err = NewSegmenter(&config.Config{StudioURL: "http://127.0.0.1:4100", StudioTimeout: 45 * time.Second})
	require.NoError(t, err)
	assert.IsType(t, &studio.HTTPSegmenter{}, seg)

	_, err = NewSegmenter(&config.Config{StudioURL: "127.0.0.1:4100"})
	require.Error(t, err)
}
