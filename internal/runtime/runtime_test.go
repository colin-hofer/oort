package runtime

import "testing"

func TestParseHost(t *testing.T) {
	app, tenant, ok := ParseHost("sales--acme.apps.example.test:8443", "apps.example.test")
	if !ok || app != "sales" || tenant != "acme" {
		t.Fatalf("unexpected host mapping: %q %q %t", app, tenant, ok)
	}
	for _, host := range []string{"cloud.example.test", "sales--acme.evil.test", "sales.apps.example.test"} {
		if _, _, ok := ParseHost(host, "apps.example.test"); ok {
			t.Fatalf("invalid app host accepted: %s", host)
		}
	}
}
