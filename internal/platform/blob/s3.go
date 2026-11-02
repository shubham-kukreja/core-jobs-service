package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

// ErrTooLarge is returned when an object exceeds Put's maxBytes.
var ErrTooLarge = errors.New("blob: object exceeds size limit")

// ErrInvalid is returned for malformed keys, URIs and content types, and for
// URIs naming a bucket other than the store's.
var ErrInvalid = errors.New("blob: invalid key or uri")

// ErrPublicBucket is returned by Verify when the bucket grants anonymous access.
var ErrPublicBucket = errors.New("blob: bucket policy allows public access")

const uriScheme = "blob://"

// partSize is the multipart chunk for streamed uploads of unknown size; it
// bounds per-upload memory.
const partSize = 8 << 20

// maxKeyLen is S3's object key limit.
const maxKeyLen = 1024

var (
	keyRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/\-]*$`)
	contentTypeRE = regexp.MustCompile(`^[a-z]+/[a-z0-9.+\-]{1,100}$`)
)

// S3Options configures an S3-compatible store.
type S3Options struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseTLS    bool
	// ServerSideEncryption requests SSE-S3 on every object. Required in
	// staging and production; local stores that lack SSE may disable it.
	ServerSideEncryption bool
}

// S3 implements Store on any S3-compatible service. Objects are private:
// the store never sets ACLs or policies and Verify refuses public buckets.
type S3 struct {
	c      *minio.Client
	bucket string
	sse    encrypt.ServerSide
}

var _ Store = (*S3)(nil)

// NewS3 returns an S3 store. It does not contact the service; call Verify at
// startup.
func NewS3(o S3Options) (*S3, error) {
	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("blob: endpoint and bucket are required")
	}
	c, err := minio.New(o.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(o.AccessKey, o.SecretKey, ""),
		Secure: o.UseTLS,
		Region: o.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: client: %w", err)
	}
	s := &S3{c: c, bucket: o.Bucket}
	if o.ServerSideEncryption {
		s.sse = encrypt.NewSSE()
	}
	return s, nil
}

// Verify checks the bucket exists and its policy grants no anonymous access.
func (s *S3) Verify(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("blob: bucket check: %w", err)
	}
	if !ok {
		return fmt.Errorf("blob: bucket %q does not exist", s.bucket)
	}
	policy, err := s.c.GetBucketPolicy(ctx, s.bucket)
	if err != nil && minio.ToErrorResponse(err).Code != "NoSuchBucketPolicy" {
		return fmt.Errorf("blob: bucket policy: %w", err)
	}
	if isPublicPolicy(policy) {
		return ErrPublicBucket
	}
	return nil
}

// isPublicPolicy reports whether a bucket policy names the anonymous principal.
func isPublicPolicy(policy string) bool {
	p := strings.Join(strings.Fields(policy), "")
	return strings.Contains(p, `"Principal":"*"`) || strings.Contains(p, `"AWS":"*"`) || strings.Contains(p, `"AWS":["*"]`)
}

// Put streams r to key, failing with ErrTooLarge past maxBytes.
func (s *S3) Put(ctx context.Context, key, contentType string, r io.Reader, maxBytes int64) (Object, error) {
	if !ValidKey(key) || !contentTypeRE.MatchString(contentType) || maxBytes <= 0 {
		return Object{}, ErrInvalid
	}
	h := sha256.New()
	lr := &limitedReader{r: io.TeeReader(r, h), max: maxBytes}
	info, err := s.c.PutObject(ctx, s.bucket, key, lr, -1, minio.PutObjectOptions{
		ContentType:          contentType,
		ServerSideEncryption: s.sse,
		PartSize:             partSize,
	})
	if err != nil {
		// A failed upload must not leave a partial object behind.
		_ = s.c.RemoveObject(context.WithoutCancel(ctx), s.bucket, key, minio.RemoveObjectOptions{})
		if lr.exceeded {
			return Object{}, ErrTooLarge
		}
		return Object{}, fmt.Errorf("blob: put: %w", err)
	}
	return Object{URI: uriScheme + s.bucket + "/" + key, Key: key, Size: info.Size, SHA256: h.Sum(nil), ContentType: contentType}, nil
}

// Get opens the object at uri.
func (s *S3) Get(ctx context.Context, uri string) (io.ReadCloser, Object, error) {
	key, err := s.parseURI(uri)
	if err != nil {
		return nil, Object{}, err
	}
	st, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, fmt.Errorf("blob: stat: %w", err)
	}
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Object{}, fmt.Errorf("blob: get: %w", err)
	}
	return obj, Object{URI: uri, Key: key, Size: st.Size, ContentType: st.ContentType}, nil
}

// Delete removes the object at uri.
func (s *S3) Delete(ctx context.Context, uri string) error {
	key, err := s.parseURI(uri)
	if err != nil {
		return err
	}
	if err := s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("blob: delete: %w", err)
	}
	return nil
}

// parseURI returns the key of a blob:// URI in this store's bucket. URIs for
// other buckets are rejected so a stored URI cannot be used to reach them.
func (s *S3) parseURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, uriScheme)
	if !ok {
		return "", ErrInvalid
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || bucket != s.bucket || !ValidKey(key) {
		return "", ErrInvalid
	}
	return key, nil
}

// ValidKey reports whether key is a safe object key: no traversal segments,
// no empty segments, no leading or trailing slash.
func ValidKey(key string) bool {
	if len(key) > maxKeyLen || !keyRE.MatchString(key) || strings.HasSuffix(key, "/") || strings.Contains(key, "//") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// limitedReader fails once more than max bytes have been read.
type limitedReader struct {
	r        io.Reader
	max, n   int64
	exceeded bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		l.exceeded = true
		return 0, ErrTooLarge
	}
	return n, err
}
