package secretbox

import "testing"

func TestRoundTrip(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := box.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Open(ciphertext, nonce)
	if err != nil || got != "secret" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}
