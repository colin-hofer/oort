package server

import (
	"database/sql"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"oort/internal/db"
)

var invitationPage = template.Must(template.New("invitation").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{if .Invitation}}Join {{.Invitation.TenantSlug}}{{else}}Invitation unavailable{{end}} · Oort</title>
<style>body{font:16px system-ui,sans-serif;background:#f6f7f8;color:#17202a;margin:0;padding:3rem 1rem}main{box-sizing:border-box;max-width:34rem;margin:auto;background:white;border:1px solid #dfe3e7;border-radius:12px;padding:2rem}h1{font-size:1.6rem;margin:.3rem 0 1rem}p{line-height:1.5}.detail{background:#f5f6f7;border-radius:8px;padding:1rem;margin:1.5rem 0}.detail p{margin:.25rem 0}.muted{color:#5f6b76}.button{border:0;border-radius:7px;background:#17202a;color:white;font:inherit;font-weight:650;padding:.75rem 1rem;cursor:pointer}</style>
</head><body><main><div class="muted">Oort membership invitation</div>
{{if .Invitation}}<h1>Join {{.Invitation.TenantSlug}}</h1><p>You were invited to join this tenant.</p>
<div class="detail"><p><strong>Email:</strong> {{.Invitation.Email}}</p><p><strong>Role:</strong> {{.Invitation.Role}}</p><p><strong>Expires:</strong> {{.Expires}}</p></div>
<form method="post"><button class="button" type="submit">Accept invitation</button></form>
{{else}}<h1>Invitation unavailable</h1><p>{{.Message}}</p>{{end}}
</main></body></html>`))

type invitationPageData struct {
	Invitation *db.Invitation
	Expires    string
	Message    string
}

func (s *Server) showInvitation(w http.ResponseWriter, r *http.Request) {
	s.invitationHeaders(w)
	if r.URL.RawQuery != "" {
		s.writeInvitationError(w, http.StatusBadRequest, "This invitation link is invalid.")
		return
	}
	invitation, err := db.GetInvitationByToken(r.Context(), s.database, r.PathValue("token"))
	if errors.Is(err, sql.ErrNoRows) {
		s.writeInvitationError(w, http.StatusNotFound, "This invitation link is invalid.")
		return
	}
	if invitationUnavailable(err) {
		s.writeInvitationError(w, http.StatusGone, "This invitation expired, was revoked, or was already used.")
		return
	}
	if err != nil {
		s.writeInvitationError(w, http.StatusInternalServerError, "The invitation could not be checked.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = invitationPage.Execute(w, invitationPageData{Invitation: &invitation, Expires: invitation.ExpiresAt.Format("January 2, 2006 at 3:04 PM MST")})
}

func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	s.invitationHeaders(w)
	if r.URL.RawQuery != "" {
		s.writeInvitationError(w, http.StatusBadRequest, "This invitation link is invalid.")
		return
	}
	_, err := db.GetInvitationByToken(r.Context(), s.database, r.PathValue("token"))
	if errors.Is(err, sql.ErrNoRows) {
		s.writeInvitationError(w, http.StatusNotFound, "This invitation link is invalid.")
		return
	}
	if invitationUnavailable(err) {
		s.writeInvitationError(w, http.StatusGone, "This invitation expired, was revoked, or was already used.")
		return
	}
	if err != nil {
		s.writeInvitationError(w, http.StatusInternalServerError, "The invitation could not be checked.")
		return
	}
	if s.config.LocalAuth {
		if !loopbackRemote(r.RemoteAddr) {
			s.writeInvitationError(w, http.StatusForbidden, "Local invitations may only be accepted from this machine.")
			return
		}
		user, tenant, err := db.AcceptLocalInvitation(r.Context(), s.database, r.PathValue("token"), requestIDFrom(r))
		if invitationUnavailable(err) {
			s.writeInvitationError(w, http.StatusGone, "This invitation expired, was revoked, or was already used.")
			return
		}
		if errors.Is(err, db.ErrConflict) {
			s.writeInvitationError(w, http.StatusConflict, "This identity is already a member.")
			return
		}
		if err != nil {
			s.writeInvitationError(w, http.StatusInternalServerError, "The invitation could not be accepted.")
			return
		}
		session, err := db.CreateControlSession(r.Context(), s.database, db.FullPrincipal(user), 12*time.Hour)
		if err != nil {
			s.writeInvitationError(w, http.StatusInternalServerError, "A browser session could not be created.")
			return
		}
		s.setControlCookie(w, session, 12*time.Hour)
		http.Redirect(w, r, invitedDashboardPath(tenant.Slug), http.StatusFound)
		return
	}
	if s.oidc == nil {
		s.writeInvitationError(w, http.StatusServiceUnavailable, "Sign-in is not configured.")
		return
	}
	nonce, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	state, err := db.CreateInvitationOIDCAttempt(r.Context(), s.database, r.PathValue("token"), nonce, verifier, 5*time.Minute)
	if errors.Is(err, sql.ErrNoRows) || invitationUnavailable(err) {
		s.writeInvitationError(w, http.StatusGone, "This invitation expired, was revoked, or was already used.")
		return
	}
	if err != nil {
		s.writeInvitationError(w, http.StatusInternalServerError, "Sign-in could not be started.")
		return
	}
	target := s.oidc.oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) invitationURL(r *http.Request, secret string) (string, error) {
	base := ""
	if !s.config.LocalAuth {
		base = strings.TrimRight(s.config.PublicURL, "/")
	}
	if base == "" {
		if !loopbackRemote(r.RemoteAddr) {
			return "", errors.New("a public URL or loopback control origin is required")
		}
		host := ""
		if loopbackHost(s.config.ControlHost) {
			if _, port, err := net.SplitHostPort(s.config.Listen); err == nil && port != "" && port != "0" {
				host = net.JoinHostPort(s.config.ControlHost, port)
			}
		}
		if host == "" && loopbackHost(requestHost(r.Host)) {
			host = r.Host
		}
		if host == "" {
			return "", errors.New("a public URL or loopback control origin is required")
		}
		base = (&url.URL{Scheme: "http", Host: host}).String()
	}
	return base + "/auth/invitations/" + url.PathEscape(secret), nil
}

func invitedDashboardPath(tenant string) string {
	return "/?" + url.Values{"tenant": []string{tenant}}.Encode()
}

func invitationUnavailable(err error) bool {
	return errors.Is(err, db.ErrInvitationAccepted) || errors.Is(err, db.ErrInvitationExpired) || errors.Is(err, db.ErrInvitationRevoked)
}

func (s *Server) invitationHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func (s *Server) writeInvitationError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = invitationPage.Execute(w, invitationPageData{Message: message})
}
