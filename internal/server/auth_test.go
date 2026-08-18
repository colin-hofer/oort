package server

import "testing"

func TestValidateCLIReturn(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:1234/callback", "http://[::1]:4321/callback", "http://localhost:9876/callback"} {
		if err := validateCLIReturn(value); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"https://127.0.0.1:1234", "http://example.com:1234", "http://127.0.0.1/callback", "http://user@127.0.0.1:1"} {
		if err := validateCLIReturn(value); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}
