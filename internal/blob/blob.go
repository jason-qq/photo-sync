// Package blob abstracts object storage for photo renditions. Clients upload
// directly to presigned URLs, so rendition bytes never pass through the API.
// Local is a disk-backed stand-in for S3 used in local development and tests.
package blob

import (
	"context"
	"time"
)

// Store is the object-storage interface used by the API.
type Store interface {
	// PresignPut returns a URL that accepts a single HTTP PUT of the object
	// at key until ttl elapses.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignGet returns a URL that serves the object at key via HTTP GET
	// until ttl elapses. It does not check that the object exists.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Exists reports whether an object is stored at key.
	Exists(ctx context.Context, key string) (bool, error)
}
