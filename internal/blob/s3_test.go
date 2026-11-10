package blob

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"
)

// TestS3 runs the blob contract against a local S3-compatible server. It
// is skipped unless PHOTOSYNC_S3_TEST_ENDPOINT is set, e.g. with RustFS:
//
//	docker run --rm -p 9000:9000 -e RUSTFS_ACCESS_KEY=testkey -e RUSTFS_SECRET_KEY=testsecret123 rustfs/rustfs
//	PHOTOSYNC_S3_TEST_ENDPOINT=http://localhost:9000 \
//	PHOTOSYNC_S3_TEST_ACCESS_KEY=testkey PHOTOSYNC_S3_TEST_SECRET_KEY=testsecret123 \
//	go test ./internal/blob
//
// Each subtest uses its own bucket, emptied and deleted afterwards. Never
// point this at real AWS.
func TestS3(t *testing.T) {
	client := testS3Client(t)
	t.Parallel() // the expiry test sleeps
	testContract(t, func(t *testing.T) Store { return NewS3(client, newBucket(t, client)) })
}

// S3 upload URLs sign the Content-Type, so the uploader must send exactly
// ContentType. (Local doesn't check it.)
func TestS3ContentTypeSigned(t *testing.T) {
	client := testS3Client(t)
	s := NewS3(client, newBucket(t, client))
	url := presignPut(t, s, key, time.Minute)

	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT as text/html = %d, want 403", resp.StatusCode)
	}
}

// Guards against an SDK change: the SDK adds body checksums to PutObject
// by default, and if one were signed into a presigned upload URL it would
// be the checksum of the (empty) presign-time body, rejecting every real
// upload. The current SDK leaves presigned URLs without one; if this
// fails, set o.RequestChecksumCalculation =
// aws.RequestChecksumCalculationWhenRequired in NewS3Client.
func TestS3PresignedPutHasNoChecksum(t *testing.T) {
	client := testS3Client(t)
	url := presignPut(t, NewS3(client, "bucket"), key, time.Minute)
	if lower := strings.ToLower(url); strings.Contains(lower, "checksum") {
		t.Errorf("upload URL carries a checksum: %s", url)
	}
}

// testS3Client returns a client for the local S3-compatible test server,
// skipping the test if none is configured.
func testS3Client(t *testing.T) *s3.Client {
	t.Helper()
	endpoint := os.Getenv("PHOTOSYNC_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("PHOTOSYNC_S3_TEST_ENDPOINT not set")
	}
	if strings.Contains(endpoint, "amazonaws.com") {
		t.Fatal("PHOTOSYNC_S3_TEST_ENDPOINT must be a local S3-compatible server, not AWS")
	}
	return NewS3Client(aws.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("PHOTOSYNC_S3_TEST_ACCESS_KEY"), os.Getenv("PHOTOSYNC_S3_TEST_SECRET_KEY"), ""),
	}, endpoint)
}

// newBucket creates a uniquely named bucket, emptied and deleted when the
// test ends.
func newBucket(t *testing.T, client *s3.Client) string {
	t.Helper()
	bucket := "test-" + strings.ToLower(ulid.Make().String())
	if err := CreateBucket(context.Background(), client, bucket); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteBucket(client, bucket) })
	return bucket
}

func deleteBucket(client *s3.Client, bucket string) {
	ctx := context.Background()
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			break
		}
		for _, obj := range page.Contents {
			client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: obj.Key})
		}
	}
	client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
}
