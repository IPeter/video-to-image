package ids

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewAndValid(t *testing.T) {
	id := NewJob()
	assert.True(t, strings.HasPrefix(id, "job_"))
	assert.Len(t, id, 4+26)
	assert.True(t, Valid(PrefixJob, id))
	assert.False(t, Valid(PrefixImage, id))
	assert.False(t, Valid(PrefixJob, "job_nope"))
	assert.False(t, Valid(PrefixJob, "job_"+strings.Repeat("Z", 26))) // overflows the ULID time range
	assert.NotEqual(t, NewJob(), NewJob())
}

func TestShort(t *testing.T) {
	assert.Equal(t, "ABCDEFGH", Short("job_01J0000000000000ABCDEFGH"))
	assert.Equal(t, "abc", Short("job_abc"))
}
