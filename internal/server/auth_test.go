package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

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

func TestLoggedOutAppHandoffStartsLogin(t *testing.T) {
	server := newHandler(&Server{})
	request := httptest.NewRequest(http.MethodGet, "https://control.example.test/auth/apps/acme/sales", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("app handoff returned %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("app handoff security headers = %v", response.Header())
	}
	target, err := url.Parse(response.Header().Get("Location"))
	if err != nil || target.Path != "/auth/login" || target.Query().Get("app_tenant") != "acme" ||
		target.Query().Get("app") != "sales" {
		t.Fatalf("app handoff location = %q, %v", response.Header().Get("Location"), err)
	}

	request = httptest.NewRequest(http.MethodGet, "https://control.example.test/auth/apps/not_valid/sales", nil)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("invalid app handoff returned %d", response.Code)
	}
}

func TestControlURL(t *testing.T) {
	if got := controlURL(Config{PublicURL: "https://control.example.test/"}); got != "https://control.example.test" {
		t.Fatalf("production control URL = %q", got)
	}
	if got := controlURL(Config{LocalAuth: true, ControlHost: "127.0.0.1", Listen: "127.0.0.1:8080"}); got != "http://127.0.0.1:8080" {
		t.Fatalf("local control URL = %q", got)
	}
	if got := controlURL(Config{LocalAuth: true, ControlHost: "example.test", Listen: "127.0.0.1:8080"}); got != "" {
		t.Fatalf("non-loopback local control URL = %q", got)
	}
}

func TestInvitationURLUsesLocalControlOrigin(t *testing.T) {
	server := &Server{config: Config{LocalAuth: true, Listen: "127.0.0.1:8080", ControlHost: "127.0.0.1", PublicURL: "https://production.example"}}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5173/v1/tenants/acme/members", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	link, err := server.invitationURL(request, "secret")
	if err != nil || link != "http://127.0.0.1:8080/auth/invitations/secret" {
		t.Fatalf("invitationURL() = %q, %v", link, err)
	}
	if path := invitedDashboardPath("acme labs"); path != "/?tenant=acme+labs" {
		t.Fatalf("invitedDashboardPath() = %q", path)
	}
	server.config = Config{PublicURL: "https://production.example/control"}
	request.RemoteAddr = "192.0.2.10:43210"
	link, err = server.invitationURL(request, "secret")
	if err != nil || link != "https://production.example/control/auth/invitations/secret" {
		t.Fatalf("production invitationURL() = %q, %v", link, err)
	}
}
