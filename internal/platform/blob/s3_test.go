package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestValidKey(t *testing.T) {
	for _, k := range []string{"uploads/2026/11/a.csv", "raw/fred/x-1_2.json", "a"} {
		if !ValidKey(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []string{"", "/abs", "a/../b", "..", "a/./b", "a//b", "a/", "a b", "a\x00b", strings.Repeat("a", 1025)} {
		if ValidKey(k) {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func TestParseURI(t *testing.T) {
	s := &S3{bucket: "core"}
	if k, err := s.parseURI("blob://core/uploads/a.csv"); err != nil || k != "uploads/a.csv" {
		t.Fatalf("got %q, %v", k, err)
	}
	for _, u := range []string{"s3://core/a", "blob://other/a", "blob://core", "blob://core/../a", "https://core/a"} {
		if _, err := s.parseURI(u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", u, err)
		}
	}
}

func TestLimitedReader(t *testing.T) {
	lr := &limitedReader{r: strings.NewReader("12345"), max: 5}
	if b, err := io.ReadAll(lr); err != nil || string(b) != "12345" {
		t.Fatalf("at limit: %q %v", b, err)
	}
	lr = &limitedReader{r: strings.NewReader("123456"), max: 5}
	if _, err := io.ReadAll(lr); !errors.Is(err, ErrTooLarge) || !lr.exceeded {
		t.Fatalf("over limit: %v", err)
	}
}

func TestIsPublicPolicy(t *testing.T) {
	public := `{"Statement":[{"Effect":"Allow","Principal": "*","Action":["s3:GetObject"]}]}`
	publicAWS := `{"Statement":[{"Principal":{"AWS":["*"]}}]}`
	private := `{"Statement":[{"Principal":{"AWS":["arn:aws:iam::1:role/app"]}}]}`
	if !isPublicPolicy(public) || !isPublicPolicy(publicAWS) || isPublicPolicy(private) || isPublicPolicy("") {
		t.Fatal("isPublicPolicy misclassifies")
	}
}

// TestS3RoundTrip runs against a real S3-compatible store when
// TEST_S3_ENDPOINT (and TEST_S3_BUCKET, TEST_S3_ACCESS_KEY, TEST_S3_SECRET_KEY) are set.
func TestS3RoundTrip(t *testing.T) {
	ep := os.Getenv("TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	s, err := NewS3(S3Options{Endpoint: ep, Bucket: os.Getenv("TEST_S3_BUCKET"),
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY"),
		ServerSideEncryption: os.Getenv("TEST_S3_SSE") == "true"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte("month,yield\nJan-2026,6.72\n")
	obj, err := s.Put(ctx, "test/roundtrip.csv", "text/csv", bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(body); !bytes.Equal(obj.SHA256, want[:]) || obj.Size != int64(len(body)) {
		t.Fatalf("object = %+v", obj)
	}
	rc, got, err := s.Get(ctx, obj.URI)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(b, body) || got.ContentType != "text/csv" {
		t.Fatalf("get = %q %+v %v", b, got, err)
	}
	if _, err := s.Put(ctx, "test/big.csv", "text/csv", bytes.NewReader(make([]byte, 1025)), 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize put: %v", err)
	}
	if _, _, err := s.Get(ctx, "blob://"+s.bucket+"/test/big.csv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oversize object must not persist: %v", err)
	}
	if err := s.Delete(ctx, obj.URI); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, obj.URI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
