// Package objstore stores run artifacts (transcripts, logs) in any
// S3-compatible store, or a local directory when no store is configured.
package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/mauza/ai-flow/internal/config"
)

type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
}

var ErrNotFound = errors.New("object not found")

func New(ctx context.Context, cfg config.ObjectStore, dataDir string) (Store, error) {
	switch cfg.Type {
	case "", "local":
		return &local{root: filepath.Join(dataDir, "objects")}, nil
	case "s3", "garage":
		ak, sk := config.Secret(cfg.AccessKeyEnv), config.Secret(cfg.SecretKeyEnv)
		if cfg.Type == "garage" {
			token := config.Secret(cfg.AdminTokenEnv)
			if cfg.AdminEndpoint == "" || token == "" {
				return nil, fmt.Errorf("objectStore: garage needs adminEndpoint and %s", cfg.AdminTokenEnv)
			}
			var err error
			ak, sk, err = bootstrapGarage(ctx, cfg.AdminEndpoint, token, "ai-flow", cfg.Bucket)
			if err != nil {
				return nil, fmt.Errorf("objectStore: %w", err)
			}
		}
		if ak == "" || sk == "" {
			return nil, fmt.Errorf("objectStore: %s and %s must be set", cfg.AccessKeyEnv, cfg.SecretKeyEnv)
		}
		c, err := minio.New(cfg.Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(ak, sk, ""),
			Secure: cfg.Secure,
			Region: cfg.Region,
		})
		if err != nil {
			return nil, err
		}
		s := &s3{c: c, bucket: cfg.Bucket}
		exists, err := c.BucketExists(ctx, cfg.Bucket)
		if err != nil {
			return nil, fmt.Errorf("objectStore: %w", err)
		}
		if !exists {
			if err := c.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
				return nil, fmt.Errorf("objectStore: create bucket: %w", err)
			}
		}
		return s, nil
	}
	return nil, fmt.Errorf("objectStore: unknown type %q", cfg.Type)
}

type local struct{ root string }

func (l *local) path(key string) (string, error) {
	p := filepath.Join(l.root, filepath.FromSlash(key))
	if !strings.HasPrefix(p, filepath.Clean(l.root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("bad key %q", key)
	}
	return p, nil
}

func (l *local) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (l *local) Get(_ context.Context, key string) ([]byte, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

type s3 struct {
	c      *minio.Client
	bucket string
}

func (s *s3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *s3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	b, err := io.ReadAll(obj)
	if err != nil {
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return b, nil
}
