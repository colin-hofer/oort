package storage

import (
	"net/url"
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
