package media

import (
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSniffRealHeaders feeds Sniff the first 4 KB of files written by ffmpeg.
func TestSniffRealHeaders(t *testing.T) {
	needFFmpeg(t)
	vid := []string{"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=0.5"}
	h264 := []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p"}
	gen := func(name string, extra ...string) string {
		return ffmpegGen(t, name, append(append([]string{}, vid...), extra...)...)
	}

	tests := []struct {
		name string
		path string
		want Format
		ok   bool
	}{
		{"mp4 (isom)", gen("a.mp4", h264...), FormatMP4, true},
		{"mp4 faststart", gen("fs.mp4", append(h264, "-movflags", "+faststart")...), FormatMP4, true},
		{"mov (qt  )", gen("a.mov", h264...), FormatMOV, true},
		{"m4v (M4V )", gen("a.m4v", h264...), FormatMP4, true},
		{"3gp", gen("a.3gp", h264...), FormatMP4, true},
		{"webm", gen("a.webm", "-c:v", "libvpx", "-b:v", "100k"), FormatWebM, true},
		{"mkv", gen("a.mkv", h264...), FormatMKV, true},
		{"avi", gen("a.avi", "-c:v", "mjpeg"), Format{}, false},
		{"mpeg-ts", gen("a.ts", h264...), Format{}, false},
		{"gif", gen("a.gif"), Format{}, false},
		{"png", ffmpegGen(t, "a.png", "-f", "lavfi", "-i", "testsrc2=size=64x64", "-frames:v", "1"), Format{}, false},
		{"jpeg", ffmpegGen(t, "a.jpg", "-f", "lavfi", "-i", "testsrc2=size=64x64", "-frames:v", "1"), Format{}, false},
		{"m4a audio only", ffmpegGen(t, "a.m4a", "-f", "lavfi", "-i", "sine=duration=0.5", "-c:a", "aac"), Format{}, false},
		{"wav", ffmpegGen(t, "a.wav", "-f", "lavfi", "-i", "sine=duration=0.5"), Format{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Sniff(readHead(t, tt.path))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSniffSynthetic(t *testing.T) {
	random := make([]byte, SniffBytes)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range random {
		random[i] = byte(r.UintN(256))
	}
	ebml := func(docType string) []byte {
		b := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0x82, 0x80 | byte(len(docType))}
		return append(b, docType...)
	}

	tests := []struct {
		name string
		head []byte
		want Format
		ok   bool
	}{
		{"empty", nil, Format{}, false},
		{"too short", []byte("\x00\x00\x00\x14ftyp"), Format{}, false},
		{"text", []byte("hello, this is definitely not a video file\n"), Format{}, false},
		{"random bytes", random, Format{}, false},
		{"iPhone mov: qt   with 20-byte ftyp", ftyp("qt  ", "qt  "), FormatMOV, true},
		{"mp4 with 20-byte ftyp (filetype would guess mov)", ftyp("mp42"), FormatMP4, true},
		{"android mp4", ftyp("mp42", "isom", "mp42"), FormatMP4, true},
		{"iso6 dash", ftyp("iso6", "iso6", "dash"), FormatMP4, true},
		{"avc1", ftyp("avc1", "isom"), FormatMP4, true},
		{"sony xavc", ftyp("XAVC", "XAVC", "mp42", "iso2"), FormatMP4, true},
		{"3gpp", ftyp("3gp5", "3gp5", "isom"), FormatMP4, true},
		{"3gpp2", ftyp("3g2a", "3g2a"), FormatMP4, true},
		{"unknown major, isom compatible", ftyp("abcd", "abcd", "isom"), FormatMP4, true},
		{"unknown major, qt compatible", ftyp("abcd", "qt  "), FormatMOV, true},
		{"unknown major, nothing known", ftyp("abcd", "efgh"), Format{}, false},
		{"heic image", ftyp("heic", "mif1", "heic"), Format{}, false},
		{"avif image", ftyp("avif", "avif", "mif1", "miaf", "MA1B"), Format{}, false},
		{"heif sequence (msf1)", ftyp("msf1", "msf1", "iso8", "hevc"), Format{}, false},
		{"unknown major with image + isom compat", ftyp("abcd", "mif1", "isom"), Format{}, false},
		{"m4a audio", ftyp("M4A ", "M4A ", "mp42", "isom"), Format{}, false},
		{"audiobook", ftyp("M4B ", "M4B ", "mp42"), Format{}, false},
		{"canon raw", ftyp("crx ", "crx ", "isom"), Format{}, false},
		{"classic quicktime moov first", box("moov", 64), FormatMOV, true},
		{"classic quicktime mdat first", box("mdat", 64), FormatMOV, true},
		{"wide atom then mdat", append(box("wide", 8), box("mdat", 32)...), FormatMOV, true},
		{"free atom then junk", append(box("free", 8), box("junk", 32)...), Format{}, false},
		{"webm", ebml("webm"), FormatWebM, true},
		{"matroska", ebml("matroska"), FormatMKV, true},
		{"ebml with unknown doctype", ebml("foo"), Format{}, false},
		{"avi", append([]byte("RIFF\x00\x10\x00\x00AVI LIST"), make([]byte, 32)...), Format{}, false},
		{"png", append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 32)...), Format{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Sniff(tt.head)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ftyp builds an ftyp box (major, minor version 0, compatible brands) followed by an
// mdat box, like the start of a real file.
func ftyp(major string, compat ...string) []byte {
	size := 16 + 4*len(compat)
	b := make([]byte, 0, size+16)
	b = binary.BigEndian.AppendUint32(b, uint32(size))
	b = append(b, "ftyp"...)
	b = append(b, major...)
	b = binary.BigEndian.AppendUint32(b, 0)
	for _, c := range compat {
		b = append(b, c...)
	}
	return append(b, box("mdat", 16)...)
}

func box(typ string, size int) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(size))
	b = append(b, typ...)
	return append(b, make([]byte, size-8)...)
}

func readHead(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	head := make([]byte, SniffBytes)
	n, err := io.ReadFull(f, head)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		require.NoError(t, err)
	}
	return head[:n]
}
