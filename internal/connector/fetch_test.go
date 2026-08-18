package connector

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchPaginationAndPointers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "two" {
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":2}]},"next":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":1}]},"next":"two"}`))
	}))
	defer server.Close()
	var output bytes.Buffer
	result, err := Fetch(context.Background(), Config{URL: server.URL, RecordsPointer: "/data/items", CursorParameter: "cursor", NextCursorPointer: "/next", AllowPrivate: true}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows != 2 || strings.Count(output.String(), "\n") != 2 {
		t.Fatalf("result = %#v, output = %q", result, output.String())
	}
}

func TestRejectsPrivateTarget(t *testing.T) {
	var output bytes.Buffer
	_, err := Fetch(context.Background(), Config{URL: "http://127.0.0.1/data"}, &output)
	if err == nil {
		t.Fatal("expected private target rejection")
	}
}

func TestPublicIPRejectsReservedRanges(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "2001:db8::1"} {
		if publicIP(net.ParseIP(value)) {
			t.Fatalf("reserved address %s was accepted", value)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) || !publicIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public address was rejected")
	}
}
