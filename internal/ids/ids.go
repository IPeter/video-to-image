// Package ids creates prefixed ULIDs (job_, img_, cf_). Job IDs double as the access
// handle for a result in Phase 1, so the random part comes from crypto/rand.
package ids

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	PrefixJob       = "job"
	PrefixImage     = "img"
	PrefixCandidate = "cf"
)

// New returns prefix + "_" + a new ULID.
func New(prefix string) string {
	return prefix + "_" + ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

func NewJob() string       { return New(PrefixJob) }
func NewImage() string     { return New(PrefixImage) }
func NewCandidate() string { return New(PrefixCandidate) }

// Valid reports whether id is prefix_ followed by a well-formed ULID. Handlers use it to
// reject junk path parameters before touching the database.
func Valid(prefix, id string) bool {
	rest, ok := strings.CutPrefix(id, prefix+"_")
	if !ok || len(rest) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.ParseStrict(rest)
	return err == nil
}

// Short returns the last 8 characters of the ULID part (the random end), used in
// human-facing file names such as cars_7K2M9QXA_1_front_three_quarter_00-06.jpg.
func Short(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}
