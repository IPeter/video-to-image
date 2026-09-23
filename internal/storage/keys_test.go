package storage

import (
	"mime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/config"
)

func TestValidKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"cars/jobs/job_1/original.mp4", true},
		{"cars/jobs/job_1/images/img_01J.jpg", true},
		{"manifest.json", true},
		{".hidden", true},
		{"", false},
		{"/cars/x.jpg", false},
		{"cars/", false},
		{"..", false},
		{"../x", false},
		{"a/../b", false},
		{"a/..", false},
		{"a..b", false}, // storage.go: keys containing ".." are rejected outright
		{`a\b`, false},
		{"a\x00b", false},
		{"a//b", false},
		{".", false},
		{"./a", false},
		{"a/./b", false},
	} {
		assert.Equal(t, tc.want, validKey(tc.key), "%q", tc.key)
	}
}

func TestValidPrefix(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		want   bool
	}{
		{"cars/jobs/job_1/", true},
		{"cars/", true},
		{"cars/jobs/job_1/images/img_", true},
		{"c", true},
		{"", false},
		{"/", false},
		{"//", false},
		{"/cars/", false},
		{"../", false},
		{"cars//", false},
		{"cars/../", false},
		{`cars\`, false},
	} {
		assert.Equal(t, tc.want, validPrefix(tc.prefix), "%q", tc.prefix)
	}
}

func TestKeyHelpers(t *testing.T) {
	assert.Equal(t, "cars/jobs/job_1/", JobPrefix("cars", "job_1"))
	for _, k := range []string{
		OriginalKey("cars", "job_1", "mp4"), PreviewKey("cars", "job_1"), ImageKey("cars", "job_1", "img_1"),
		ThumbKey("cars", "job_1", "img_1"), ManifestKey("cars", "job_1"), ZipKey("cars", "job_1"),
	} {
		assert.True(t, validKey(k), k)
	}
	assert.True(t, validPrefix(JobPrefix("cars", "job_1")))
}

func TestContentTypeForKey(t *testing.T) {
	for key, want := range map[string]string{
		"a/original.mp4":  "video/mp4",
		"a/original.mov":  "video/quicktime",
		"a/original.webm": "video/webm",
		"a/original.mkv":  "video/x-matroska",
		"a/img.jpg":       "image/jpeg",
		"a/IMG.JPEG":      "image/jpeg",
		"a/manifest.json": "application/json",
		"a/export.zip":    "application/zip",
		"a/noext":         "application/octet-stream",
		"a.dir/noext":     "application/octet-stream",
		"a/x.unknownext1": "application/octet-stream",
	} {
		assert.Equal(t, want, ContentTypeForKey(key), key)
	}
	// Extensions outside the built-in table fall back to the stdlib.
	assert.Equal(t, mime.TypeByExtension(".css"), ContentTypeForKey("a/site.css"))
}

func TestAttachmentDisposition(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", "attachment"},
		{"cars_7K2M9QXA_images.zip", `attachment; filename="cars_7K2M9QXA_images.zip"`},
		{"with space.jpg", `attachment; filename="with space.jpg"`},
		{"../../etc/passwd", `attachment; filename=".._.._etc_passwd"`},
		{`a\b.zip`, `attachment; filename="a_b.zip"`},
		{`say "hi".zip`, `attachment; filename="say _hi_.zip"; filename*=UTF-8''say%20%22hi%22.zip`},
		{"line\r\nbreak.zip", `attachment; filename="line__break.zip"; filename*=UTF-8''line%0D%0Abreak.zip`},
		{"autó.zip", `attachment; filename="aut_.zip"; filename*=UTF-8''aut%C3%B3.zip`},
	} {
		got := AttachmentDisposition(tc.name)
		assert.Equal(t, tc.want, got, "%q", tc.name)
		if tc.name == "" {
			continue
		}
		// The value must parse as a media type with the expected (possibly sanitised) name.
		disp, params, err := mime.ParseMediaType(got)
		require.NoError(t, err, got)
		assert.Equal(t, "attachment", disp)
		assert.NotEmpty(t, params["filename"])
	}
}

func TestOpen(t *testing.T) {
	ctx := t.Context()

	st, err := Open(ctx, config.StorageConfig{Backend: "local", LocalDir: t.TempDir()})
	require.NoError(t, err)
	assert.IsType(t, &Local{}, st)

	st, err = Open(ctx, config.StorageConfig{
		Backend: "s3", S3Endpoint: "http://127.0.0.1:1", S3Region: "us-east-1", S3Bucket: "b",
		S3AccessKeyID: "k", S3SecretKey: "s", S3UsePathStyle: true,
	})
	require.NoError(t, err, "NewS3 must not contact the endpoint")
	assert.IsType(t, &S3{}, st)
	_, err = st.PresignGet(ctx, "a/b.jpg", 0, "")
	assert.ErrorIs(t, err, ErrPresignUnsupported, "presigning is off unless PresignDownloads is set")

	_, err = Open(ctx, config.StorageConfig{Backend: "s3"})
	assert.Error(t, err, "bucket required")

	_, err = Open(ctx, config.StorageConfig{Backend: "gcs"})
	assert.ErrorContains(t, err, `unknown backend "gcs"`)
	_, err = Open(ctx, config.StorageConfig{})
	assert.Error(t, err)
}
