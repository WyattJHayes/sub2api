package repository

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	defaultRadarArtifactPresignExpiry = 15 * time.Minute
	maxRadarArtifactPresignExpiry     = 24 * time.Hour
)

// S3EvaluationArtifactObjectStore stores Radar evidence in AWS S3 or an
// S3-compatible service such as MinIO or Cloudflare R2.
type S3EvaluationArtifactObjectStore struct {
	client        *s3.Client
	presigner     *s3.PresignClient
	bucket        string
	prefix        string
	presignExpiry time.Duration
}

var _ service.EvaluationArtifactObjectStore = (*S3EvaluationArtifactObjectStore)(nil)

func NewS3EvaluationArtifactObjectStore(ctx context.Context, cfg *config.RadarArtifactStorageConfig) (*S3EvaluationArtifactObjectStore, error) {
	if cfg == nil || !cfg.Active() {
		return nil, service.ErrArtifactObjectStoreUnavailable
	}
	client, err := newS3Client(ctx, s3ClientParams{
		Endpoint:        strings.TrimSpace(cfg.Endpoint),
		Region:          strings.TrimSpace(cfg.Region),
		AccessKeyID:     strings.TrimSpace(cfg.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(cfg.SecretAccessKey),
		ForcePathStyle:  cfg.ForcePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("create Radar artifact S3 client: %w", err)
	}
	expiry := time.Duration(cfg.PresignExpiry) * time.Second
	if expiry <= 0 {
		expiry = defaultRadarArtifactPresignExpiry
	}
	if expiry > maxRadarArtifactPresignExpiry {
		return nil, fmt.Errorf("radar artifact presign expiry exceeds %s", maxRadarArtifactPresignExpiry)
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3EvaluationArtifactObjectStore{
		client:        client,
		presigner:     s3.NewPresignClient(client),
		bucket:        strings.TrimSpace(cfg.Bucket),
		prefix:        prefix,
		presignExpiry: expiry,
	}, nil
}

func (s *S3EvaluationArtifactObjectStore) effectiveExpiry(expiry time.Duration) time.Duration {
	if expiry <= 0 {
		expiry = s.presignExpiry
	}
	if expiry <= 0 {
		expiry = defaultRadarArtifactPresignExpiry
	}
	if expiry > maxRadarArtifactPresignExpiry {
		return maxRadarArtifactPresignExpiry
	}
	return expiry
}

func (s *S3EvaluationArtifactObjectStore) objectKey(value string) (string, error) {
	if s == nil || s.client == nil || s.presigner == nil || strings.TrimSpace(s.bucket) == "" {
		return "", service.ErrArtifactObjectStoreUnavailable
	}
	key := strings.TrimSpace(value)
	if key == "" || strings.ContainsRune(key, '\x00') {
		return "", service.ErrArtifactInvalid
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", service.ErrArtifactInvalid
	}
	if s.prefix != "" && !strings.HasPrefix(key, s.prefix) {
		key = s.prefix + key
	}
	return key, nil
}

func (s *S3EvaluationArtifactObjectStore) PresignPut(ctx context.Context, request service.ArtifactObjectPutRequest, expiry time.Duration) (*service.ArtifactObjectUpload, error) {
	key, err := s.objectKey(request.ObjectKey)
	if err != nil {
		return nil, err
	}
	if request.Bytes <= 0 || request.Bytes > 1024*1024*1024 || strings.TrimSpace(request.MIMEType) == "" || !validArtifactSHA256(request.SHA256) {
		return nil, service.ErrArtifactInvalid
	}
	mimeType := strings.TrimSpace(request.MIMEType)
	sha256Value := strings.TrimSpace(request.SHA256)
	sha256Bytes, err := hex.DecodeString(sha256Value)
	if err != nil || len(sha256Bytes) != sha256.Size {
		return nil, service.ErrArtifactInvalid
	}
	checksumSHA256 := base64.StdEncoding.EncodeToString(sha256Bytes)
	ifNoneMatch := "*"
	length := request.Bytes
	result, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         &s.bucket,
		Key:            &key,
		ContentType:    &mimeType,
		ContentLength:  &length,
		ChecksumSHA256: &checksumSHA256,
		IfNoneMatch:    &ifNoneMatch,
		Metadata:       map[string]string{"sha256": sha256Value},
	}, s3.WithPresignExpires(s.effectiveExpiry(expiry)))
	if err != nil {
		return nil, fmt.Errorf("presign Radar artifact upload: %w", err)
	}
	return &service.ArtifactObjectUpload{
		URL: result.URL,
		Headers: map[string]string{
			"Content-Type":          mimeType,
			"Content-Length":        fmt.Sprintf("%d", request.Bytes),
			"X-Amz-Meta-Sha256":     sha256Value,
			"X-Amz-Checksum-Sha256": checksumSHA256,
			"If-None-Match":         ifNoneMatch,
		},
		ExpiresAt: time.Now().UTC().Add(s.effectiveExpiry(expiry)),
	}, nil
}

func (s *S3EvaluationArtifactObjectStore) Head(ctx context.Context, objectKey string) (*service.ArtifactObjectMetadata, error) {
	key, err := s.objectKey(objectKey)
	if err != nil {
		return nil, err
	}
	finish := servertiming.ObserveDependency(ctx, "s3")
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	finish()
	if err != nil {
		return nil, fmt.Errorf("head Radar artifact object: %w", err)
	}
	if result.ContentLength == nil || result.ContentType == nil {
		return nil, fmt.Errorf("%w: object length or MIME type is missing", service.ErrArtifactObjectMetadataUnavailable)
	}
	sha256Value, err := artifactSHA256FromHead(result)
	if err != nil {
		return nil, err
	}
	etag := strings.Trim(strings.TrimSpace(artifactStringValueOrEmpty(result.ETag)), "\"")
	return &service.ArtifactObjectMetadata{
		ObjectKey: key,
		Bytes:     *result.ContentLength,
		MIMEType:  strings.TrimSpace(*result.ContentType),
		SHA256:    sha256Value,
		ETag:      etag,
	}, nil
}

func artifactSHA256FromHead(result *s3.HeadObjectOutput) (string, error) {
	for key, value := range result.Metadata {
		if strings.EqualFold(strings.TrimSpace(key), "sha256") && validArtifactSHA256(value) {
			return strings.TrimSpace(value), nil
		}
	}
	if result.ChecksumSHA256 != nil {
		decoded, err := base64.StdEncoding.DecodeString(*result.ChecksumSHA256)
		if err == nil && len(decoded) == 32 {
			return hex.EncodeToString(decoded), nil
		}
	}
	return "", fmt.Errorf("%w: sha256 metadata is missing or invalid", service.ErrArtifactObjectMetadataUnavailable)
}

func artifactStringValueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *S3EvaluationArtifactObjectStore) PresignGet(ctx context.Context, objectKey string, expiry time.Duration) (string, time.Time, error) {
	key, err := s.objectKey(objectKey)
	if err != nil {
		return "", time.Time{}, err
	}
	result, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(s.effectiveExpiry(expiry)))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign Radar artifact read: %w", err)
	}
	return result.URL, time.Now().UTC().Add(s.effectiveExpiry(expiry)), nil
}

func (s *S3EvaluationArtifactObjectStore) Delete(ctx context.Context, objectKey string) error {
	key, err := s.objectKey(objectKey)
	if err != nil {
		return err
	}
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	finish()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("delete Radar artifact object: %w", err)
	}
	return nil
}

func (s *S3EvaluationArtifactObjectStore) Open(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	key, err := s.objectKey(objectKey)
	if err != nil {
		return nil, err
	}
	finish := servertiming.ObserveDependency(ctx, "s3")
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	finish()
	if err != nil {
		return nil, fmt.Errorf("open Radar artifact object: %w", err)
	}
	return result.Body, nil
}
