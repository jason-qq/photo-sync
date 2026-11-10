package blob

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ContentType is the type every rendition is stored and served as. S3
// upload URLs sign it, so uploads must send exactly this Content-Type.
const ContentType = "image/jpeg"

// S3 stores objects in an S3 bucket (or an S3-compatible server such as
// MinIO). Clients upload and download directly with presigned URLs, so the
// bucket stays private and object bytes never pass through the API.
//
// Unlike Local, S3 presigned PUT URLs cannot limit the object size; a
// leaked upload URL could store a large object until it expires (15 min).
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

var _ Store = (*S3)(nil)

// NewS3 returns an S3 store for bucket, which must already exist.
func NewS3(client *s3.Client, bucket string) *S3 {
	return &S3{client: client, presign: s3.NewPresignClient(client), bucket: bucket}
}

// NewS3Client returns an S3 client from cfg. endpoint, if set, points at
// an S3-compatible server (e.g. RustFS) and switches to path-style URLs
// (http://host/bucket/key), which such servers expect.
func NewS3Client(cfg aws.Config, endpoint string) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
}

func (s *S3) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		ContentType: aws.String(ContentType),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob: presign put %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
		// Matches Local: the browser may cache an image only privately,
		// and not much longer than the URL is useful.
		ResponseCacheControl: aws.String("private, max-age=300"),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob: presign get %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("blob: head %s: %w", key, err)
}

// isNotFound reports whether err means the object doesn't exist. HEAD
// responses have no body, so S3 reports "NotFound"; some S3-compatible
// servers use "NoSuchKey".
func isNotFound(err error) bool {
	var nf *types.NotFound
	var nsk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey")
}

// CreateBucket creates bucket if it doesn't exist. Intended for local
// S3-compatible servers and tests; deployed buckets are created (private,
// with lifecycle rules) by infrastructure code.
func CreateBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	var owned *types.BucketAlreadyOwnedByYou
	if err == nil || errors.As(err, &owned) {
		return nil
	}
	return fmt.Errorf("create bucket %s: %w", bucket, err)
}
