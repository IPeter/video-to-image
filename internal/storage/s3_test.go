package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S3 tests run against a real S3-compatible service (the docker compose MinIO) when
// V2I_TEST_S3_ENDPOINT is set, e.g. http://localhost:59000. Bucket and credentials
// default to the compose values.
func testS3Options(t *testing.T) S3Options {
	t.Helper()
	endpoint := os.Getenv("V2I_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("V2I_TEST_S3_ENDPOINT not set")
	}
	return S3Options{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		Bucket:       getenv("V2I_TEST_S3_BUCKET", "v2i"),
		AccessKeyID:  getenv("V2I_TEST_S3_ACCESS_KEY_ID", "v2i-minio"),
		SecretKey:    getenv("V2I_TEST_S3_SECRET_ACCESS_KEY", "v2i-minio-secret"),
		UsePathStyle: true,
		Presign:      true,
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newTestS3(t *testing.T, o S3Options) *S3 {
	t.Helper()
	st, err := NewS3(t.Context(), o)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, st.Ping(ctx), "S3 endpoint %s bucket %s must be reachable", o.Endpoint, o.Bucket)
	return st
}

// testPrefix returns a random prefix that is removed when the test ends.
func testPrefix(t *testing.T, st Storage) string {
	t.Helper()
	prefix := "test-" + randomHex(t, 6) + "/"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		assert.NoError(t, st.DeletePrefix(ctx, prefix), "cleanup")
	})
	return prefix
}

func TestS3Conformance(t *testing.T) {
	runConformance(t, newTestS3(t, testS3Options(t)))
}

func TestS3PresignGet(t *testing.T) {
	o := testS3Options(t)
	st := newTestS3(t, o)
	prefix := testPrefix(t, st)
	key := prefix + "cars/jobs/job_1/export.zip"
	putBytes(t, st, key, []byte("zip bytes"), "application/zip")

	type fetched struct {
		StatusCode int
		Header     http.Header
	}
	fetch := func(t *testing.T, rawURL string) (fetched, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return fetched{StatusCode: resp.StatusCode, Header: resp.Header}, string(body)
	}

	t.Run("download name", func(t *testing.T) {
		u, err := st.PresignGet(t.Context(), key, time.Minute, "cars_7K2M9QXA_images.zip")
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		assert.Equal(t, "60", parsed.Query().Get("X-Amz-Expires"))

		resp, body := fetch(t, u)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Equal(t, "zip bytes", body)
		assert.Equal(t, `attachment; filename="cars_7K2M9QXA_images.zip"`, resp.Header.Get("Content-Disposition"))
		assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	})

	t.Run("non-ascii name", func(t *testing.T) {
		u, err := st.PresignGet(t.Context(), key, time.Minute, "autó.zip")
		require.NoError(t, err)
		resp, body := fetch(t, u)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Equal(t, AttachmentDisposition("autó.zip"), resp.Header.Get("Content-Disposition"))
	})

	t.Run("inline", func(t *testing.T) {
		u, err := st.PresignGet(t.Context(), key, 0, "")
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		assert.Equal(t, "900", parsed.Query().Get("X-Amz-Expires"), "ttl <= 0 means 15 minutes")
		resp, body := fetch(t, u)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Empty(t, resp.Header.Get("Content-Disposition"))
	})

	t.Run("ttl capped at 7 days", func(t *testing.T) {
		u, err := st.PresignGet(t.Context(), key, 30*24*time.Hour, "")
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprint(int((7 * 24 * time.Hour).Seconds())), parsed.Query().Get("X-Amz-Expires"))
	})

	t.Run("tampered signature", func(t *testing.T) {
		u, err := st.PresignGet(t.Context(), key, time.Minute, "x.zip")
		require.NoError(t, err)
		u = strings.Replace(u, "x.zip", "y.zip", 1) // the disposition is part of the signed query
		resp, _ := fetch(t, u)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("public endpoint", func(t *testing.T) {
		if !strings.Contains(o.Endpoint, "localhost") {
			t.Skip("needs a localhost endpoint to derive a second host name")
		}
		po := o
		po.PublicEndpoint = strings.Replace(o.Endpoint, "localhost", "127.0.0.1", 1)
		pst := newTestS3(t, po)
		u, err := pst.PresignGet(t.Context(), key, time.Minute, "a.zip")
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		pub, err := url.Parse(po.PublicEndpoint)
		require.NoError(t, err)
		assert.Equal(t, pub.Host, parsed.Host, "presigned URLs use the public endpoint")
		resp, body := fetch(t, u)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Equal(t, "zip bytes", body)
	})

	t.Run("disabled", func(t *testing.T) {
		do := o
		do.Presign = false
		dst, err := NewS3(t.Context(), do)
		require.NoError(t, err)
		_, err = dst.PresignGet(t.Context(), key, time.Minute, "x.zip")
		assert.ErrorIs(t, err, ErrPresignUnsupported)
	})
}

func TestS3DeletePrefixBatches(t *testing.T) {
	st := newTestS3(t, testS3Options(t))
	st.deleteBatch = 3 // exercise several DeleteObjects calls plus a partial last batch
	prefix := testPrefix(t, st)
	for i := range 8 {
		putBytes(t, st, fmt.Sprintf("%sjob/images/img_%d.jpg", prefix, i), []byte{byte(i)}, "image/jpeg")
	}
	putBytes(t, st, prefix+"other/keep.jpg", []byte("k"), "image/jpeg")

	require.NoError(t, st.DeletePrefix(t.Context(), prefix+"job/"))
	for i := range 8 {
		assert.False(t, exists(t, st, fmt.Sprintf("%sjob/images/img_%d.jpg", prefix, i)))
	}
	assert.True(t, exists(t, st, prefix+"other/keep.jpg"))
}

func TestS3MissingBucket(t *testing.T) {
	o := testS3Options(t)
	o.Bucket = "v2i-missing-" + randomHex(t, 4)
	st, err := NewS3(t.Context(), o)
	require.NoError(t, err)
	assert.Error(t, st.Ping(t.Context()))

	// A missing bucket is a misconfiguration, not a missing object.
	_, err = st.Get(t.Context(), "a/b.jpg")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}

func TestS3BadCredentials(t *testing.T) {
	o := testS3Options(t)
	o.SecretKey = "wrong-secret"
	st, err := NewS3(t.Context(), o)
	require.NoError(t, err)
	assert.Error(t, st.Ping(t.Context()))
	_, err = st.Get(t.Context(), "a/b.jpg")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}

func TestS3PresignDisabledOffline(t *testing.T) {
	st, err := NewS3(t.Context(), S3Options{
		Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKeyID: "k", SecretKey: "s", UsePathStyle: true,
	})
	require.NoError(t, err)
	_, err = st.PresignGet(t.Context(), "a/b.jpg", time.Minute, "")
	assert.ErrorIs(t, err, ErrPresignUnsupported)
	_, err = st.PresignGet(t.Context(), "../b.jpg", time.Minute, "")
	assert.ErrorIs(t, err, ErrInvalidKey)

	_, err = NewS3(t.Context(), S3Options{})
	assert.Error(t, err, "bucket required")
}

func TestS3PresignOffline(t *testing.T) {
	// Presigning is pure computation: no endpoint needs to be up.
	st, err := NewS3(t.Context(), S3Options{
		Endpoint: "http://minio:9000", PublicEndpoint: "https://s3.example.test", Bucket: "v2i",
		AccessKeyID: "k", SecretKey: "s", UsePathStyle: true, Presign: true,
	})
	require.NoError(t, err)
	u, err := st.PresignGet(t.Context(), "cars/jobs/job_1/export.zip", time.Hour, "cars_X_images.zip")
	require.NoError(t, err)
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "s3.example.test", parsed.Host)
	assert.Equal(t, "/v2i/cars/jobs/job_1/export.zip", parsed.Path)
	q := parsed.Query()
	assert.Equal(t, "3600", q.Get("X-Amz-Expires"))
	assert.Equal(t, `attachment; filename="cars_X_images.zip"`, q.Get("response-content-disposition"))
	assert.NotEmpty(t, q.Get("X-Amz-Signature"))
}

func TestS3StreamingPutUsesMultipartAndAbortsOnFailure(t *testing.T) {
	st := newTestS3(t, testS3Options(t))
	prefix := testPrefix(t, st)

	// A 20 MiB stream of unknown length goes up in three 8 MiB-max parts.
	pr, pw := io.Pipe()
	go func() {
		_, err := pw.Write(pseudoRandom(9, 20<<20))
		pw.CloseWithError(err)
	}()
	require.NoError(t, st.Put(t.Context(), prefix+"big.zip", pr, "application/zip"))
	head, err := st.client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(st.bucket), Key: aws.String(prefix + "big.zip"),
	})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(aws.ToString(head.ETag), `-3"`), "multipart ETag, got %s", aws.ToString(head.ETag))

	// A stream failing after the first parts leaves no incomplete upload behind.
	pr, pw = io.Pipe()
	go func() {
		_, _ = pw.Write(pseudoRandom(10, 17<<20))
		pw.CloseWithError(errors.New("zip writer failed"))
	}()
	require.Error(t, st.Put(t.Context(), prefix+"broken.zip", pr, "application/zip"))
	ups, err := st.client.ListMultipartUploads(t.Context(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(st.bucket), Prefix: aws.String(prefix),
	})
	require.NoError(t, err)
	assert.Empty(t, ups.Uploads, "the failed multipart upload must be aborted")
}
