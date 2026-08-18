package queryexec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateQueryAndParameters(t *testing.T) {
	sqlText, types, err := Validate("SELECT name FROM customers WHERE active = $active LIMIT $limit;", map[string]any{
		"active": true,
		"limit":  json.Number("50"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(sqlText, ";") || types["active"] != "boolean" || types["limit"] != "integer" {
		t.Fatalf("unexpected validated query: %q %#v", sqlText, types)
	}
	for _, test := range []struct {
		name string
		sql  string
		args map[string]any
	}{
		{"mutation", "SELECT 1; DELETE FROM customers", nil},
		{"extension", "INSTALL httpfs", nil},
		{"missing parameter", "SELECT $limit", nil},
		{"extra parameter", "SELECT 1", map[string]any{"limit": 1.0}},
		{"dollar quote", "SELECT $$DELETE FROM customers$$", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := Validate(test.sql, test.args); err == nil {
				t.Fatal("unsafe or invalid query was accepted")
			}
		})
	}
	if _, _, err := Validate("SELECT 'DELETE', 1 /* UPDATE */", nil); err != nil {
		t.Fatalf("keywords inside literals/comments were rejected: %v", err)
	}
}

func TestRequestFraming(t *testing.T) {
	request := Request{SQL: "SELECT $limit", Parameters: map[string]any{"limit": 5}, MaxRows: 10, MaxBytes: 1024}
	encoded, err := EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRequest(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SQL != request.SQL || decoded.Parameters["limit"].(json.Number).String() != "5" {
		t.Fatalf("framing changed request: %#v", decoded)
	}
}

func TestQueryEnvironmentDropsAmbientCredentials(t *testing.T) {
	clean := scrubEnvironment([]string{
		"PATH=/usr/bin", "OORT_DATABASE_URL=secret", "AWS_SECRET_ACCESS_KEY=secret",
		"PGPASSWORD=secret", "HTTPS_PROXY=http://proxy", "LANG=C.UTF-8",
	})
	joined := strings.Join(clean, "\n")
	if joined != "PATH=/usr/bin\nLANG=C.UTF-8" {
		t.Fatalf("unexpected child environment: %q", joined)
	}
}

func BenchmarkRequestFraming(b *testing.B) {
	request := Request{SQL: "SELECT * FROM customers LIMIT $limit", Parameters: map[string]any{"limit": 50}, MaxRows: 10_000, MaxBytes: 10 << 20}
	for b.Loop() {
		encoded, err := EncodeRequest(request)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := DecodeRequest(bytes.NewReader(encoded)); err != nil {
			b.Fatal(err)
		}
	}
}
