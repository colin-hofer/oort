package db

import "testing"

func TestNormalizeEmail(t *testing.T) {
	email, err := NormalizeEmail("  Teammate+Dev@Example.COM ")
	if err != nil || email != "teammate+dev@example.com" {
		t.Fatalf("NormalizeEmail() = %q, %v", email, err)
	}
	for _, value := range []string{"", "not-an-email", "Name <name@example.com>"} {
		if _, err := NormalizeEmail(value); err == nil {
			t.Fatalf("NormalizeEmail(%q) succeeded", value)
		}
	}
}
