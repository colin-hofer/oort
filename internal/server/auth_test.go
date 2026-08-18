package server

import (
	"net/http"
	"net/http/httptest"
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
