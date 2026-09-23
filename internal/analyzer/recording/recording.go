// Package recording records real model responses to disk and replays them offline
// (V2I_ANALYZER=recording / recorded). Recordings make eval re-runs of the scoring and
// selection steps free and deterministic (plan/08-roadmap.md).
//
// Each successful call with a non-empty Request.CacheKey is stored as one JSON file,
// <dir>/<FileName(cacheKey)>, holding the raw model text and its usage. A later call
// with the same key overwrites it, so the file always holds the latest answer.
package recording

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
)

// Record is the on-disk format of one recording.
type Record struct {
	CacheKey string `json:"cacheKey"`
	Step     string `json:"step"`
	Model    string `json:"model"` // model requested; Usage.Model is what the provider reported
	Text     string `json:"text"`
	Usage    Usage  `json:"usage"`
	// PromptSHA256 fingerprints the system prompt, text parts, media MIME types and the
	// response schema. The replayer warns when a request no longer matches it (the
	// domain prompt changed since recording), but still replays.
	PromptSHA256 string    `json:"promptSha256,omitempty"`
	RecordedAt   time.Time `json:"recordedAt"`
}

// Usage mirrors analyzer.Usage with JSON-friendly field names.
type Usage struct {
	Model          string `json:"model"`
	InputTokens    int    `json:"inputTokens"`
	OutputTokens   int    `json:"outputTokens"`
	ThinkingTokens int    `json:"thinkingTokens"`
	LatencyMs      int64  `json:"latencyMs"`
}

func usageFrom(u analyzer.Usage) Usage {
	return Usage{
		Model:          u.Model,
		InputTokens:    u.InputTokens,
		OutputTokens:   u.OutputTokens,
		ThinkingTokens: u.ThinkingTokens,
		LatencyMs:      u.Latency.Milliseconds(),
	}
}

func (u Usage) analyzer() analyzer.Usage {
	return analyzer.Usage{
		Model:          u.Model,
		InputTokens:    u.InputTokens,
		OutputTokens:   u.OutputTokens,
		ThinkingTokens: u.ThinkingTokens,
		Latency:        time.Duration(u.LatencyMs) * time.Millisecond,
	}
}

// maxBaseLen keeps file names well below the common 255-byte limit.
const maxBaseLen = 160

// FileName maps a cache key to a safe file name ("<name>.json"). Characters outside
// [A-Za-z0-9._-] become "_". When anything had to change, or the key contains
// upper-case letters (case-insensitive file systems such as the macOS default would
// otherwise merge "Key" and "key"), or it is longer than maxBaseLen, a "+<hash>" suffix
// of the key's SHA-256 is appended. "+" never occurs in an unchanged name, so distinct
// keys never map to the same file.
func FileName(cacheKey string) string {
	var b strings.Builder
	b.Grow(len(cacheKey))
	changed := false
	for _, r := range cacheKey {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
			changed = true
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	name := b.String()
	if len(name) > maxBaseLen {
		name = name[:maxBaseLen]
		changed = true
	}
	if changed || name == "" {
		sum := sha256.Sum256([]byte(cacheKey))
		name += "+" + hex.EncodeToString(sum[:8])
	}
	return name + ".json"
}

// Path returns the recording file for cacheKey inside dir.
func Path(dir, cacheKey string) string { return filepath.Join(dir, FileName(cacheKey)) }

// promptFingerprint hashes everything that shapes the model's answer except the media
// bytes themselves (which the cache key already identifies).
func promptFingerprint(req *analyzer.Request) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(s), s) // hash.Hash writes never fail
	}
	write(req.Step)
	write(req.Model)
	write(req.SystemPrompt)
	for _, p := range req.Parts {
		if p.Text != "" {
			write("text")
			write(p.Text)
		} else {
			write("media")
			write(p.MIMEType)
		}
	}
	if req.ResponseSchema != nil {
		s, err := json.Marshal(req.ResponseSchema)
		if err == nil {
			write(string(s))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeAtomic writes data to path via a temp file in the same directory + rename, so a
// crash or a concurrent reader never sees a half-written recording.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".rec-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
