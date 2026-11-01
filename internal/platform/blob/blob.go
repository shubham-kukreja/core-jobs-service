// Package blob is the S3-compatible object store contract for raw payloads,
// uploads and large job results.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("blob: not found")

// Object describes a stored object.
type Object struct {
	URI         string // blob://<bucket>/<key>; never a presigned or public URL
	Key         string
	Size        int64
	SHA256      []byte
	ContentType string
}

// Store persists immutable objects. Implementations must enable server-side
// encryption and must never make objects public.
type Store interface {
	// Put stores r under key and returns its size and sha256. Put fails if the
	// object would exceed maxBytes.
	Put(ctx context.Context, key, contentType string, r io.Reader, maxBytes int64) (Object, error)
	Get(ctx context.Context, uri string) (io.ReadCloser, Object, error)
	Delete(ctx context.Context, uri string) error
}
