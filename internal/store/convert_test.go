package store

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNullText(t *testing.T) {
	assert.Nil(t, nullText(""))
	require.NotNil(t, nullText("x"))
	assert.Equal(t, "x", *nullText("x"))
	assert.Equal(t, "", textOrEmpty(nil))
	s := "y"
	assert.Equal(t, "y", textOrEmpty(&s))
}

func TestNumericRoundTrip(t *testing.T) {
	for _, f := range []float64{0, 0.1, 0.000001, 1.234567, 9999.999999, -0.5, 1e-9} {
		n, err := numericFromFloat(f)
		require.NoError(t, err, f)
		got, err := floatFromNumeric(n)
		require.NoError(t, err)
		assert.Equal(t, f, got)
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := numericFromFloat(f)
		assert.Error(t, err, f)
	}
	got, err := floatFromNumeric(pgtype.Numeric{})
	require.NoError(t, err)
	assert.Zero(t, got, "NULL numeric reads as 0")
}

func TestF64ShortestFromFloat32(t *testing.T) {
	for _, f := range []float64{12.3, 0.1, 3599.125, 1.5, 0, -7.25} {
		assert.Equal(t, f, f64(f32(f)), f)
	}
	assert.True(t, math.IsNaN(f64(float32(math.NaN()))))
	assert.True(t, math.IsInf(f64(float32(math.Inf(1))), 1))
}

func TestInt32Of(t *testing.T) {
	v, err := int32Of("x", 42)
	require.NoError(t, err)
	assert.Equal(t, int32(42), v)
	_, err = int32Of("x", math.MaxInt32+1)
	assert.ErrorContains(t, err, "x ")
	_, err = int32Of("x", math.MinInt32-1)
	assert.Error(t, err)
}

func TestPgText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"", ""},
		{"a\x00b", "a�b"},
		{"bad\xffutf8", "bad�utf8"},
		{"héllo €", "héllo €"},
	}
	for _, tt := range tests {
		got := pgText(tt.in)
		assert.Equal(t, tt.want, got, "%q", tt.in)
		assert.True(t, utf8.ValidString(got))
	}
}

func TestTailUTF8(t *testing.T) {
	assert.Equal(t, "abc", tailUTF8("abc", 10))
	assert.Equal(t, "bc", tailUTF8("abc", 2))
	assert.Equal(t, "", tailUTF8("abc", 0))
	// "€" is 3 bytes: a 4-byte tail of "a€€" would start inside the first "€".
	assert.Equal(t, "€", tailUTF8("a€€", 4))
	assert.Equal(t, "€€", tailUTF8("a€€", 6))
	s := strings.Repeat("é", 1000)
	for n := 0; n < 20; n++ {
		got := tailUTF8(s, n)
		assert.True(t, utf8.ValidString(got))
		assert.LessOrEqual(t, len(got), n)
		assert.GreaterOrEqual(t, len(got), n-1)
	}
}

func TestReplaceJSONNul(t *testing.T) {
	b, err := jsonb(map[string]string{"k": "a\x00b"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"k":"a�b"}`, string(b))

	// A literal backslash followed by "u0000" is text, not a NUL escape.
	b, err = jsonb(`\u0000`)
	require.NoError(t, err)
	var back string
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, `\u0000`, back)

	// Escaped backslash then a real NUL.
	b, err = jsonb("\\\x00")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, "\\�", back)
}

func TestJSONBObject(t *testing.T) {
	var nilMap map[string]any
	var nilPtr *struct{ A int }
	for _, v := range []any{nil, nilMap, nilPtr} {
		b, err := jsonbObject(v)
		require.NoError(t, err)
		assert.Equal(t, "{}", string(b))
	}
	b, err := jsonbObject(struct {
		Candidates int `json:"candidates"`
	}{3})
	require.NoError(t, err)
	assert.JSONEq(t, `{"candidates":3}`, string(b))
	for _, v := range []any{[]int{1}, 3, "x", true} {
		_, err := jsonbObject(v)
		assert.Error(t, err, v)
	}
	_, err = jsonbObject(map[string]any{"f": func() {}})
	assert.Error(t, err)
}

func TestDecodeHelpersNeverNil(t *testing.T) {
	for _, in := range []string{"", "null", "{}"} {
		m, err := decodeMeta([]byte(in))
		require.NoError(t, err)
		assert.NotNil(t, m, in)
	}
	for _, in := range []string{"", "null", "[]"} {
		s, err := decodeStrings([]byte(in))
		require.NoError(t, err)
		assert.NotNil(t, s, in)
		assert.Empty(t, s)
	}
	b, err := encodeStrings(nil)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(b))
}
