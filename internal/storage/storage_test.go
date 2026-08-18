package storage

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPresignPutBindsObjectAndSize(t *testing.T) {
	client, err := New(Config{
		Endpoint: "http://127.0.0.1:9000", Region: "us-east-1",
		AccessKey: "access", SecretKey: "secret", Bucket: "bucket",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }
	signed, headers, err := client.PresignPut("tenants/id/uploads/run/source.csv", 42, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/bucket/tenants/id/uploads/run/source.csv" || parsed.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("unexpected signed URL: %s", signed)
	}
	if headers.Get("Content-Length") != "42" || parsed.Query().Get("X-Amz-SignedHeaders") != "content-length;host" {
		t.Fatalf("upload size is not signed: %#v %s", headers, parsed.Query().Get("X-Amz-SignedHeaders"))
	}
}

func TestUploadStreamsSignedContent(t *testing.T) {
	client, err := New(Config{Endpoint: "http://storage.invalid", Region: "us-east-1", AccessKey: "access", SecretKey: "secret", Bucket: "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut || r.ContentLength != 5 || r.URL.Query().Get("X-Amz-Signature") == "" {
			t.Fatalf("unexpected upload request: %s length=%d url=%s", r.Method, r.ContentLength, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "hello" {
			t.Errorf("unexpected upload body %q: %v", body, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := client.Upload(context.Background(), "tenant/upload.csv", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
