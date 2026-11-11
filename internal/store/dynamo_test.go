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
// transaction touches the same item (DynamoDB Local serializes
// transactions, so it never does). item is the index of the conflicting
// item: 0 is the session item both CreatePhoto and CreateReviewer check
// (parallel uploads to one session), 1 is CreatePhoto's hash lock
// (concurrent uploads of the same file). Other requests pass through.
type conflictInjector struct {
	next     aws.HTTPClient
	item     int
	n        atomic.Int32
	injected atomic.Int32
}

func (c *conflictInjector) Do(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.Header.Get("X-Amz-Target"), ".TransactWriteItems") && c.n.Add(-1) >= 0 {
		c.injected.Add(1)
		codes := []string{"None", "None", "None"}
		codes[c.item] = "TransactionConflict"
		var reasons []string
		for _, code := range codes {
			reasons = append(reasons, `{"Code":"`+code+`"}`)
		}
		body := `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionCanceledException",` +
			`"Message":"Transaction cancelled [` + strings.Join(codes, ", ") + `]",` +
			`"CancellationReasons":[` + strings.Join(reasons, ",") + `]}`
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
	createPhoto := func(s *Dynamo, sid string) error {
		p, created, err := s.CreatePhoto(ctx, photo(sid, model.NewID()))
		if err == nil && !created {
			return errors.New("not created")
		}
		if err == nil {
			_, err = s.GetPhoto(ctx, sid, p.ID)
		}
		return err
	}
	createReviewer := func(s *Dynamo, sid string) error {
		r := reviewer(sid, model.NewID())
		if err := s.CreateReviewer(ctx, r); err != nil {
			return err
		}
		_, err := s.GetReviewer(ctx, sid, r.ID)
		return err
	}
	tests := []struct {
		name   string
		item   int
		create func(*Dynamo, string) error
	}{
		// The 500 seen in production: parallel uploads to one session.
		{"photo, session item", 0, createPhoto},
		{"photo, hash lock", 1, createPhoto},
		{"reviewer, session item", 0, createReviewer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj := &conflictInjector{next: http.DefaultClient, item: tt.item}
			db := localClient(t, inj)
			s := NewDynamo(db, newTable(t, db))
			sid := newSession(t, s)

			// Conflicts below the retry limit: retried, then created.
			inj.n.Store(maxConflictRetries - 1)
			if err := tt.create(s, sid); err != nil {
				t.Fatalf("after %d conflicts: %v", maxConflictRetries-1, err)
			}
			if got := inj.injected.Load(); got != maxConflictRetries-1 {
				t.Errorf("injected %d conflicts, want %d", got, maxConflictRetries-1)
			}

			// Persistent conflict: gives up with the DynamoDB error.
			inj.injected.Store(0)
			inj.n.Store(100)
			err := tt.create(s, sid)
			var tce *types.TransactionCanceledException
			if !errors.As(err, &tce) {
				t.Fatalf("persistent conflict: err = %v, want TransactionCanceledException", err)
			}
			if got := inj.injected.Load(); got != maxConflictRetries {
				t.Errorf("attempts = %d, want %d", got, maxConflictRetries)
			}
		})
	}
}
