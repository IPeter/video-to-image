package storage

import (
	"context"
	"fmt"

	"video-to-image/internal/config"
)

// Backend names accepted in config.StorageConfig.Backend (V2I_STORAGE).
const (
	BackendLocal = "local"
	BackendS3    = "s3"
)

// Open returns the backend selected by cfg.Backend. It does not contact S3; call Ping
// to verify the connection.
func Open(ctx context.Context, cfg config.StorageConfig) (Storage, error) {
	switch cfg.Backend {
	case BackendLocal:
		return NewLocal(cfg.LocalDir)
	case BackendS3:
		return NewS3(ctx, S3Options{
			Endpoint:       cfg.S3Endpoint,
			PublicEndpoint: cfg.S3PublicEndpoint,
			Region:         cfg.S3Region,
			Bucket:         cfg.S3Bucket,
			AccessKeyID:    cfg.S3AccessKeyID,
			SecretKey:      cfg.S3SecretKey,
			UsePathStyle:   cfg.S3UsePathStyle,
			Presign:        cfg.PresignDownloads,
		})
	default:
		return nil, fmt.Errorf("storage: unknown backend %q (want %s or %s)", cfg.Backend, BackendLocal, BackendS3)
	}
}
