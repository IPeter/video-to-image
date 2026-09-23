package export

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
)

// ManifestFile is the manifest's entry name inside the ZIP.
const ManifestFile = "manifest.json"

// FileName returns <domainKey>_<ids.Short(jobID)>_<index>_<shotId>_<mm-ss>.jpg,
// e.g. cars_7K2M9QXA_1_front_three_quarter_00-06.jpg. The shot ID is lower-cased and
// every run of characters outside [a-z0-9_-] becomes one "_" (an empty result reads
// "other"); the domain key and short job ID are reduced the same way, keeping the
// job ID's case, so a name can never carry a path separator into a ZIP. mm-ss is
// TimestampLabel with ":" replaced by "-" (h-mm-ss from one hour).
func FileName(domainKey, jobID string, index int, shotID string, timestampSec float64) string {
	shot := sanitize(strings.ToLower(shotID))
	if shot == "" {
		shot = domain.OtherShotID
	}
	return sanitize(domainKey) + "_" + sanitize(ids.Short(jobID)) + "_" + strconv.Itoa(index) + "_" +
		shot + "_" + strings.ReplaceAll(TimestampLabel(timestampSec), ":", "-") + ".jpg"
}

// ZipName returns <domainKey>_<ids.Short(jobID)>_images.zip.
func ZipName(domainKey, jobID string) string {
	return sanitize(domainKey) + "_" + sanitize(ids.Short(jobID)) + "_images.zip"
}

// maxLabelSec keeps absurd inputs from overflowing the integer maths (~31 years).
const maxLabelSec = 1e9

// TimestampLabel formats seconds as "MM:SS" (floor), or "H:MM:SS" from one hour.
// Negative and non-finite values read "00:00". A microsecond of tolerance absorbs
// float error, so 5.9999999999 (a frame at 6 s after arithmetic) reads "00:06".
func TimestampLabel(sec float64) string {
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 {
		return "00:00"
	}
	total := int64(math.Floor(math.Min(sec, maxLabelSec) + 1e-6))
	h, m, s := total/3600, total%3600/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// sanitize keeps [A-Za-z0-9_-] and replaces every run of other characters with one
// "_", trimming such replacements at either end.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for _, r := range s {
		if r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			if pending && b.Len() > 0 {
				b.WriteByte('_')
			}
			pending = false
			b.WriteRune(r)
			continue
		}
		pending = true
	}
	return b.String()
}
