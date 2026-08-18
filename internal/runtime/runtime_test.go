package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestUnauthenticatedBrowserRedirectsToControlLogin(t *testing.T) {
	server := New(Config{HostSuffix: "apps.example.test", ControlURL: "https://control.example.test"})
	request := httptest.NewRequest(http.MethodGet, "http://sales--acme.apps.example.test/", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusFound ||
		response.Header().Get("Location") != "https://control.example.test/auth/apps/acme/sales" ||
		response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unauthenticated app returned %d location=%q headers=%v", response.Code,
			response.Header().Get("Location"), response.Header())
	}
}

func TestUnauthenticatedRuntimeAPIStaysUnauthorized(t *testing.T) {
	server := New(Config{HostSuffix: "apps.example.test", ControlURL: "https://control.example.test"})
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/runtime/v1/queries/report"},
		{http.MethodPost, "/runtime/v1/queries/report"},
	} {
		request := httptest.NewRequest(test.method, "http://sales--acme.apps.example.test"+test.path, nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("Location") != "" {
			t.Fatalf("%s %s returned %d location=%q", test.method, test.path, response.Code,
				response.Header().Get("Location"))
		}
	}
}
