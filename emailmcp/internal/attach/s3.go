package attach

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/mailbuild"
)

// ObjectStore uploads attachment bytes and returns a short-lived download URL.
type ObjectStore interface {
	PutAndPresign(ctx context.Context, filename, contentType string, data []byte) (string, error)
}

// S3Store writes objects to an S3 bucket and returns presigned GET URLs.
type S3Store struct {
	bucket  string
	prefix  string
	ttl     time.Duration
	client  *s3.Client
	presign *s3.PresignClient
	logger  *slog.Logger
}

func NewS3Store(ctx context.Context, bucket, prefix string, ttl time.Duration, logger *slog.Logger) (*S3Store, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config for s3: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	return &S3Store{
		bucket:  bucket,
		prefix:  prefix,
		ttl:     ttl,
		client:  client,
		presign: s3.NewPresignClient(client),
		logger:  logger,
	}, nil
}

func (s *S3Store) PutAndPresign(ctx context.Context, filename, contentType string, data []byte) (string, error) {
	filename = mailbuild.SanitizeFilename(filename)
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	key := path.Join(s.prefix, hex.EncodeToString(rnd[:]), filename)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3 put: %w", err)
	}
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(s.ttl))
	if err != nil {
		return "", fmt.Errorf("s3 presign: %w", err)
	}
	s.logger.Info("attachment uploaded for download",
		"bucket", s.bucket, "size", len(data), "ttl", s.ttl)
	return out.URL, nil
}
