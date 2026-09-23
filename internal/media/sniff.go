package media

import (
	"encoding/binary"
	"strings"

	"github.com/h2non/filetype"
	"github.com/h2non/filetype/matchers"
)

// Accepted container formats.
var (
	FormatMP4  = Format{MIME: "video/mp4", Ext: "mp4"}
	FormatMOV  = Format{MIME: "video/quicktime", Ext: "mov"}
	FormatWebM = Format{MIME: "video/webm", Ext: "webm"}
	FormatMKV  = Format{MIME: "video/x-matroska", Ext: "mkv"}
)

// SniffBytes is how much of a file Sniff wants to see.
const SniffBytes = 4096

// Sniff detects the container from the first bytes of a file (≥ 4 KB recommended),
// trusting magic bytes over any declared MIME type. Accepts MP4, MOV, WebM and MKV.
//
// ISO BMFF files (anything with an "ftyp" box) are classified from their major and
// compatible brands rather than by h2non/filetype, whose matcher order is not
// deterministic for these: "qt  " is MOV; isom/mp4x/avc1/M4V/3gp/3g2/dash/XAVC and
// similar are MP4 (3GPP is decoded by the same demuxer, so it is treated as MP4);
// still-image (HEIF/AVIF) and audio-only (M4A/M4B) brands are rejected. Classic
// QuickTime files without ftyp (starting with a moov/mdat/wide/free/skip/pnot atom)
// are MOV. Matroska/WebM are detected from the EBML DocType. Everything else — AVI,
// MPEG-TS, images, text, empty input — is rejected.
func Sniff(head []byte) (Format, bool) {
	if f, ok, decided := sniffISOBMFF(head); decided {
		return f, ok
	}
	// WebM before MKV: both are EBML; the DocType tells them apart.
	if matchers.Webm(head) {
		return FormatWebM, true
	}
	if matchers.Mkv(head) {
		return FormatMKV, true
	}
	kind, err := filetype.Match(head)
	if err != nil {
		return Format{}, false
	}
	switch kind.MIME.Value {
	case "video/mp4", "video/x-m4v", "video/3gpp":
		return FormatMP4, true
	case "video/quicktime":
		return FormatMOV, true
	case "video/webm":
		return FormatWebM, true
	case "video/x-matroska":
		return FormatMKV, true
	}
	return Format{}, false
}

// sniffISOBMFF classifies files whose first box is an ftyp box or a classic QuickTime
// atom. decided is false when the header is not ISO BMFF at all.
func sniffISOBMFF(head []byte) (f Format, ok, decided bool) {
	if len(head) < 12 {
		return Format{}, false, false
	}
	size := int(binary.BigEndian.Uint32(head[0:4]))
	switch typ := string(head[4:8]); typ {
	case "ftyp":
		major := string(head[8:12])
		end := len(head)
		if size >= 16 && size < end {
			end = size
		}
		var compat []string
		for off := 16; off+4 <= end; off += 4 {
			compat = append(compat, string(head[off:off+4]))
		}
		f, ok := classifyBrands(major, compat)
		return f, ok, true
	case "moov", "mdat", "pnot":
		return FormatMOV, true, true
	case "wide", "free", "skip":
		// Padding atoms are only meaningful when a movie atom follows.
		if size >= 8 && size+8 <= len(head) {
			switch string(head[size+4 : size+8]) {
			case "moov", "mdat", "ftyp", "wide", "free":
				return FormatMOV, true, true
			}
		}
	}
	return Format{}, false, false
}

// classifyBrands decides from the major brand first, then the compatible brands.
func classifyBrands(major string, compat []string) (Format, bool) {
	switch {
	case imageBrands[major] || audioBrands[major]:
		return Format{}, false
	case major == "qt  ":
		return FormatMOV, true
	case isMP4Brand(major):
		return FormatMP4, true
	}
	// Unknown major brand: an image brand anywhere wins over a generic "isom".
	for _, c := range compat {
		if imageBrands[c] {
			return Format{}, false
		}
	}
	for _, c := range compat {
		if c == "qt  " {
			return FormatMOV, true
		}
		if isMP4Brand(c) {
			return FormatMP4, true
		}
	}
	return Format{}, false
}

var mp4Brands = map[string]bool{
	"isom": true, "iso2": true, "iso3": true, "iso4": true, "iso5": true, "iso6": true,
	"iso7": true, "iso8": true, "iso9": true, "isoa": true,
	"mp41": true, "mp42": true, "mp71": true, "avc1": true, "mmp4": true, "dash": true,
	"M4V ": true, "M4VH": true, "M4VP": true, "F4V ": true, "F4P ": true,
	"MSNV": true, "XAVC": true, "KDDI": true, "CAEP": true, "caqv": true,
	"NDAS": true, "NDSC": true, "NDSH": true, "NDSM": true, "NDSP": true, "NDSS": true,
	"NDXC": true, "NDXH": true, "NDXM": true, "NDXP": true, "NDXS": true, "NSDC": true,
}

// isMP4Brand reports MP4-family video brands, including every 3GPP/3GPP2 brand
// (3gp4, 3gp5, 3gp6, 3gg6, 3ge6, 3gs7, 3g2a, ...).
func isMP4Brand(b string) bool {
	return mp4Brands[b] || strings.HasPrefix(b, "3gp") || strings.HasPrefix(b, "3g2") ||
		strings.HasPrefix(b, "3ge") || strings.HasPrefix(b, "3gg") || strings.HasPrefix(b, "3gs") ||
		strings.HasPrefix(b, "3gr")
}

// imageBrands are HEIF/AVIF (and camera raw) brands: ISO BMFF, but not video.
var imageBrands = map[string]bool{
	"heic": true, "heix": true, "heim": true, "heis": true,
	"hevc": true, "hevx": true, "hevm": true, "hevs": true,
	"mif1": true, "mif2": true, "msf1": true, "miaf": true,
	"avif": true, "avis": true, "avci": true, "avcs": true,
	"MiHE": true, "MiHA": true, "MiHB": true, "MiPr": true,
	"jpeg": true, "jpgs": true, "crx ": true,
}

// audioBrands are audio-only MP4 files (iTunes audio, audiobooks, Flash audio).
var audioBrands = map[string]bool{
	"M4A ": true, "M4B ": true, "M4P ": true, "F4A ": true, "F4B ": true,
}
