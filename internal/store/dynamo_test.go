package store

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"photo-sync/internal/model"
)

// TestDynamo runs the store contract against DynamoDB Local. It is skipped
// unless PHOTOSYNC_DYNAMODB_TEST_ENDPOINT is set, e.g.:
//
//	docker run --rm -p 8000:8000 amazon/dynamodb-local
//	PHOTOSYNC_DYNAMODB_TEST_ENDPOINT=http://localhost:8000 go test ./internal/store
//
// Each subtest uses its own table, deleted afterwards. Never point this at
// a real AWS endpoint.
func TestDynamo(t *testing.T) {
	db := localClient(t, nil)
	testContract(t, func(t *testing.T) Store { return NewDynamo(db, newTable(t, db)) })
}

// localClient returns a client for DynamoDB Local, skipping the test if
// none is configured. httpClient, if non-nil, sends the requests.
func localClient(t *testing.T, httpClient aws.HTTPClient) *dynamodb.Client {
	t.Helper()
	endpoint := os.Getenv("PHOTOSYNC_DYNAMODB_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("PHOTOSYNC_DYNAMODB_TEST_ENDPOINT not set")
	}
	if strings.Contains(endpoint, "amazonaws.com") {
		t.Fatal("PHOTOSYNC_DYNAMODB_TEST_ENDPOINT must be DynamoDB Local, not AWS")
	}
	return dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:   httpClient,
	})
}

// newTable creates a uniquely named table, deleted when the test ends.
func newTable(t *testing.T, db *dynamodb.Client) string {
	t.Helper()
	table := "test-" + strings.ToLower(model.NewID())
	if err := CreateTable(context.Background(), db, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table})
	})
	return table
}

// conflictInjector fails the first n TransactWriteItems calls with the
// TransactionCanceledException real DynamoDB returns when a concurrent
// transaction holds the hash lock (DynamoDB Local serializes transactions,
// so it never does). Other requests pass through.
type conflictInjector struct {
	next     aws.HTTPClient
	n        atomic.Int32
	injected atomic.Int32
}

func (c *conflictInjector) Do(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.Header.Get("X-Amz-Target"), ".TransactWriteItems") && c.n.Add(-1) >= 0 {
		c.injected.Add(1)
		// Reasons are indexed like CreatePhoto's items: session, hash, photo.
		body := `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionCanceledException",` +
			`"Message":"Transaction cancelled [None, TransactionConflict, None]",` +
			`"CancellationReasons":[{"Code":"None"},{"Code":"TransactionConflict","Message":"Transaction is ongoing for the item"},{"Code":"None"}]}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"application/x-amz-json-1.0"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	return c.next.Do(req)
}

func TestDynamoConflictRetry(t *testing.T) {
	inj := &conflictInjector{next: http.DefaultClient}
	db := localClient(t, inj)
	s := NewDynamo(db, newTable(t, db))
	sid := newSession(t, s)

	// Conflicts below the retry limit: retried, then the photo is created.
	inj.n.Store(maxConflictRetries - 1)
	p, created, err := s.CreatePhoto(ctx, photo(sid, "h1"))
	if err != nil || !created {
		t.Fatalf("after %d conflicts: created=%v err=%v", maxConflictRetries-1, created, err)
	}
	if got := inj.injected.Load(); got != maxConflictRetries-1 {
		t.Errorf("injected %d conflicts, want %d", got, maxConflictRetries-1)
	}
	if _, err := s.GetPhoto(ctx, sid, p.ID); err != nil {
		t.Errorf("photo not stored: %v", err)
	}

	// Persistent conflict: gives up with the DynamoDB error.
	inj.injected.Store(0)
	inj.n.Store(100)
	_, _, err = s.CreatePhoto(ctx, photo(sid, "h2"))
	var tce *types.TransactionCanceledException
	if !errors.As(err, &tce) {
		t.Fatalf("persistent conflict: err = %v, want TransactionCanceledException", err)
	}
	if got := inj.injected.Load(); got != maxConflictRetries {
		t.Errorf("attempts = %d, want %d", got, maxConflictRetries)
	}
}
