package export

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteStoredEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	modified := time.Date(2026, 9, 23, 12, 34, 56, 0, time.UTC)
	data := []byte("\xff\xd8 a JPEG, byte for byte \xff\xd9")
	require.NoError(t, WriteStoredEntry(zw, "grey/a_studio-grey.jpg", data, modified))
	require.NoError(t, WriteStoredEntry(zw, "empty.jpg", nil, modified))
	require.NoError(t, zw.Close())

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	f := zr.File[0]
	assert.Equal(t, "grey/a_studio-grey.jpg", f.Name)
	assert.Equal(t, zip.Store, f.Method)
	assert.Zero(t, f.Flags&0x8, "no data descriptor")
	assert.True(t, f.Modified.Equal(modified), "modified %s", f.Modified)
	rc, err := f.Open()
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close(), "the CRC matches")
	assert.Equal(t, data, got)
	assert.Zero(t, zr.File[1].UncompressedSize64)
}
