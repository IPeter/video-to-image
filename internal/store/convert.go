package store

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// Conversions between the plain Go types of models.go and the sqlc column types.

// nullText maps "" to NULL.
func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// textOrEmpty maps NULL to "".
func textOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// utcPtr returns a copy of *t in UTC (nil stays nil).
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// numericFromFloat converts f to a numeric parameter. Postgres rounds it to the
// column's scale (numeric(10,6) → 6 decimals). NaN and ±Inf are rejected.
func numericFromFloat(f float64) (pgtype.Numeric, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return pgtype.Numeric{}, fmt.Errorf("invalid amount %v", f)
	}
	var n pgtype.Numeric
	if err := n.Scan(strconv.FormatFloat(f, 'f', -1, 64)); err != nil {
		return pgtype.Numeric{}, fmt.Errorf("amount %v: %w", f, err)
	}
	return n, nil
}

// floatFromNumeric converts a numeric column to float64 (NULL → 0).
func floatFromNumeric(n pgtype.Numeric) (float64, error) {
	f, err := n.Float64Value()
	if err != nil {
		return 0, err
	}
	return f.Float64, nil
}

// f32 narrows to a real column.
func f32(f float64) float32 { return float32(f) }

// f64 widens a real column to the shortest float64 that prints like the float32, so a
// stored 12.3 reads back as 12.3 rather than 12.300000190734863.
func f64(f float32) float64 {
	v, err := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	if err != nil {
		return float64(f)
	}
	return v
}

// int32Of checks that v fits an integer column.
func int32Of(field string, v int64) (int32, error) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("%s %d out of range for integer column", field, v)
	}
	return int32(v), nil
}

// pgText makes s acceptable to a Postgres text column: invalid UTF-8 becomes U+FFFD
// and NUL bytes (which text cannot hold) become U+FFFD too. Used for free text that
// comes from the model or from process output.
func pgText(s string) string {
	if strings.IndexByte(s, 0) < 0 && utf8.ValidString(s) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}

// tailUTF8 returns the longest suffix of s of at most n bytes that starts on a rune
// boundary.
func tailUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// jsonb marshals v for a jsonb column. Postgres jsonb rejects the \u0000 escape, so it
// is rewritten to � (json.Marshal already replaces invalid UTF-8).
func jsonb(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return replaceJSONNul(b), nil
}

// replaceJSONNul rewrites every \u0000 escape in the JSON text b to �. A
// backslash always starts a two-character escape in JSON, so skipping the escaped
// character keeps an escaped backslash followed by "u0000" (literal text) intact.
func replaceJSONNul(b []byte) []byte {
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		if i+5 < len(b) && b[i+1] == 'u' && string(b[i+2:i+6]) == "0000" {
			copy(b[i+2:i+6], "fffd")
		}
		i++ // skip the escaped character
	}
	return b
}

// jsonbObject marshals meta for a jsonb column that must hold an object: nil (or a
// value that marshals to null) becomes {}; arrays and scalars are rejected so that
// reading the column back into a map always works.
func jsonbObject(meta any) ([]byte, error) {
	if meta == nil {
		return []byte("{}"), nil
	}
	b, err := jsonb(meta)
	if err != nil {
		return nil, err
	}
	switch {
	case string(b) == "null":
		return []byte("{}"), nil
	case len(b) == 0 || b[0] != '{':
		return nil, fmt.Errorf("meta must marshal to a JSON object, got %.40s", b)
	}
	return b, nil
}

// decodeMeta decodes a jsonb object column; the result is never nil.
func decodeMeta(b []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil { // JSON null
		m = map[string]any{}
	}
	return m, nil
}

// decodeStrings decodes a jsonb string array; the result is never nil.
func decodeStrings(b []byte) ([]string, error) {
	out := []string{}
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil { // JSON null
		out = []string{}
	}
	return out, nil
}

// encodeStrings encodes a string array for jsonb; nil becomes [].
func encodeStrings(v []string) ([]byte, error) {
	if v == nil {
		v = []string{}
	}
	return jsonb(v)
}
