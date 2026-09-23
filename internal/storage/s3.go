package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	// maxDeleteBatch is the DeleteObjects limit per request.
	maxDeleteBatch = 1000
	// maxPresignTTL is the SigV4 presign limit.
	maxPresignTTL = 7 * 24 * time.Hour
	// defaultPresignTTL applies when PresignGet is called with ttl <= 0.
	defaultPresignTTL = 15 * time.Minute
	// abortTimeout bounds AbortMultipartUpload after a failed or cancelled upload; it
	// runs on a fresh context so a cancelled request still cleans up its parts.
	abortTimeout = 30 * time.Second
	// maxReportedDeleteErrors caps the per-object failures spelled out in an error.
	maxReportedDeleteErrors = 10
)

// S3Options configures NewS3. Endpoint is empty for AWS itself and set for
// S3-compatible services (MinIO: "http://minio:9000").
type S3Options struct {
	Endpoint       string // API endpoint; empty = AWS default resolution
	PublicEndpoint string // host put into presigned URLs (browser-reachable); default Endpoint
	Region         string // default us-east-1
	Bucket         string
	AccessKeyID    string // static credentials when both keys are set, else the default chain
	SecretKey      string
	UsePathStyle   bool // required by MinIO without wildcard DNS
	Presign        bool // enable PresignGet; otherwise it returns ErrPresignUnsupported
}

// S3 stores objects in one bucket of an S3-compatible service.
type S3 struct {
	client      *s3.Client
	presign     *s3.PresignClient // nil when presigning is disabled
	uploader    *transfermanager.Client
	bucket      string
	deleteBatch int
}

var _ Storage = (*S3)(nil)

// NewS3 builds the clients. It makes no network calls; use Ping to check the bucket.
//
// For a custom Endpoint (MinIO and other S3-compatible services) request checksums are
// only sent when an operation requires them and response checksums are only validated
// when required: the SDK's newer always-on CRC defaults are not implemented uniformly
// outside AWS.
func NewS3(ctx context.Context, o S3Options) (*S3, error) {
	if o.Bucket == "" {
		return nil, errors.New("storage: s3: bucket is required")
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.AccessKeyID != "" && o.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("storage: s3: load aws config: %w", err)
	}

	checksum := aws.RequestChecksumCalculationWhenSupported
	if o.Endpoint != "" {
		checksum = aws.RequestChecksumCalculationWhenRequired
	}
	clientFor := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(cfg, func(so *s3.Options) {
			if endpoint != "" {
				so.BaseEndpoint = aws.String(endpoint)
			}
			so.UsePathStyle = o.UsePathStyle
			if o.Endpoint != "" {
				so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
				so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			}
		})
	}

	s := &S3{
		client:      clientFor(o.Endpoint),
		bucket:      o.Bucket,
		deleteBatch: maxDeleteBatch,
	}
	s.uploader = transfermanager.New(s.client, func(to *transfermanager.Options) {
		to.RequestChecksumCalculation = checksum
		to.FailTimeout = abortTimeout
	})
	if o.Presign {
		pubClient := s.client
		if o.PublicEndpoint != "" && o.PublicEndpoint != o.Endpoint {
			pubClient = clientFor(o.PublicEndpoint)
		}
		s.presign = s3.NewPresignClient(pubClient)
	}
	return s, nil
}

// Bucket returns the bucket name.
func (s *S3) Bucket() string { return s.bucket }

// Put uploads r with the transfer manager: objects below its multipart threshold
// (16 MiB) are buffered and sent with one PutObject, larger or unknown-length streams
// go up as a multipart upload in 8 MiB parts, which is aborted on failure, so a failed
// Put never leaves a partial object. An empty contentType is stored as
// application/octet-stream. A Read of r that blocks is not interrupted by ctx: a
// producer writing into an io.Pipe must close it when it gives up.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	if !validKey(key) {
		return fmt.Errorf("storage: put %q: %w", key, ErrInvalidKey)
	}
	if r == nil {
		return fmt.Errorf("storage: put %q: nil reader", key)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// The transfer manager buffers parts with plain Reads that ignore ctx. Seekable
	// bodies (files, byte readers) are passed through so their length stays known;
	// streams are wrapped so a cancel stops the upload at the next Read.
	body := r
	if _, ok := r.(io.Seeker); !ok {
		body = ctxReader{ctx: ctx, r: r}
	}
	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

// Get streams the object. The body is bound to ctx.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("storage: get %q: %w", key, ErrInvalidKey)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("storage: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return out.Body, nil
}

// PresignGet signs a GET against PublicEndpoint. ttl <= 0 means 15 minutes; it is
// capped at the SigV4 maximum of 7 days. It does not check that the object exists.
func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("storage: presign %q: %w", key, ErrInvalidKey)
	}
	if s.presign == nil {
		return "", ErrPresignUnsupported
	}
	switch {
	case ttl <= 0:
		ttl = defaultPresignTTL
	case ttl > maxPresignTTL:
		ttl = maxPresignTTL
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if downloadName != "" {
		in.ResponseContentDisposition = aws.String(AttachmentDisposition(downloadName))
	}
	req, err := s.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: presign %q: %w", key, err)
	}
	return req.URL, nil
}

// Delete removes one object; S3 treats a missing key as success.
func (s *S3) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return fmt.Errorf("storage: delete %q: %w", key, ErrInvalidKey)
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

// DeletePrefix lists the prefix and removes what it finds with DeleteObjects, up to
// 1000 keys per request. Per-object failures are collected and returned together
// (the first few spelled out) after every batch has been tried.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	if !validPrefix(prefix) {
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, ErrInvalidKey)
	}
	var (
		batch  []s3types.ObjectIdentifier
		failed []deleteFailure
		errs   []error
	)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		f, err := s.deleteObjects(ctx, batch)
		failed = append(failed, f...)
		if err != nil {
			errs = append(errs, err)
		}
		batch = batch[:0]
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list: %w", err))
			break
		}
		for _, obj := range page.Contents {
			batch = append(batch, s3types.ObjectIdentifier{Key: obj.Key})
			if len(batch) >= s.deleteBatch {
				flush()
			}
		}
	}
	flush()

	if len(failed) > 0 {
		errs = append(errs, deleteFailures(failed))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("storage: delete prefix %q: %w", prefix, err)
	}
	return nil
}

type deleteFailure struct{ key, code, message string }

// deleteFailures formats per-object DeleteObjects errors.
type deleteFailures []deleteFailure

func (d deleteFailures) Error() string {
	msg := fmt.Sprintf("%d object(s) not deleted:", len(d))
	for i, f := range d {
		if i == maxReportedDeleteErrors {
			msg += fmt.Sprintf(" … and %d more", len(d)-i)
			break
		}
		msg += fmt.Sprintf(" %s (%s: %s);", f.key, f.code, f.message)
	}
	return msg
}

func (s *S3) deleteObjects(ctx context.Context, objs []s3types.ObjectIdentifier) ([]deleteFailure, error) {
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.bucket),
		Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("delete %d object(s): %w", len(objs), err)
	}
	var failed []deleteFailure
	for _, e := range out.Errors {
		if aws.ToString(e.Code) == "NoSuchKey" { // already gone
			continue
		}
		failed = append(failed, deleteFailure{
			key: aws.ToString(e.Key), code: aws.ToString(e.Code), message: aws.ToString(e.Message),
		})
	}
	return failed, nil
}

// Head returns size, content type and modification time.
func (s *S3) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("storage: head %q: %w", key, ErrInvalidKey)
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("storage: head %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: head %q: %w", key, err)
	}
	ct := aws.ToString(out.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ContentType:  ct,
		LastModified: aws.ToTime(out.LastModified).UTC(),
	}, nil
}

// Ping checks the bucket exists and the credentials can reach it.
func (s *S3) Ping(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("storage: ping bucket %q: %w", s.bucket, err)
	}
	return nil
}

// isS3NotFound recognises a missing key: the modelled NoSuchKey (GetObject) and
// NotFound (HeadObject, which has no error body) types, the same codes from services
// that return them unmodelled, and a bare 404 without any error code. NoSuchBucket is
// a misconfiguration and deliberately not a missing object.
func isS3NotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	var nf *s3types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}
	var respErr interface{ HTTPStatusCode() int }
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound
}
