package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"nebulous/internal/db"
)

type oidcAuth struct {
	issuer   string
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func newOIDCAuth(ctx context.Context, config Config) (*oidcAuth, error) {
	public, err := url.Parse(config.PublicURL)
	if err != nil || public.Scheme != "https" || public.Host == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" {
		return nil, fmt.Errorf("NEB_PUBLIC_URL must be an absolute HTTPS URL")
	}
	provider, err := oidc.NewProvider(ctx, config.OIDCIssuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC provider: %w", err)
	}
	redirect := *public
	redirect.Path = strings.TrimRight(redirect.Path, "/") + "/auth/callback"
	return &oidcAuth{
		issuer: strings.TrimRight(config.OIDCIssuer, "/"),
		oauth: oauth2.Config{ClientID: config.OIDCClientID, ClientSecret: config.OIDCSecret,
			Endpoint: provider.Endpoint(), RedirectURL: redirect.String(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"}},
		verifier: provider.Verifier(&oidc.Config{ClientID: config.OIDCClientID}),
	}, nil
}

func (s *Server) startOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, r, http.StatusNotFound, "not_configured", "OIDC login is not configured in local mode")
		return
	}
	var cliReturn *string
	if value := r.URL.Query().Get("cli_return"); value != "" {
		if err := validateCLIReturn(value); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_cli_return", err.Error())
			return
		}
		cliReturn = &value
	}
	nonce, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	state, err := db.CreateOIDCAttempt(r.Context(), s.database, nonce, verifier, cliReturn, 5*time.Minute)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "login could not be started")
		return
	}
	target := s.oidc.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) finishOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		writeError(w, r, http.StatusUnauthorized, "oidc_rejected", "the identity provider rejected the login")
		return
	}
	attempt, err := db.ConsumeOIDCAttempt(r.Context(), s.database, r.URL.Query().Get("state"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusBadRequest, "invalid_login", "the login attempt expired or was already used")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "login verification failed")
		return
	}
	token, err := s.oidc.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(attempt.CodeVerifier))
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "oidc_exchange_failed", "the identity provider code could not be exchanged")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "oidc_invalid", "the identity provider did not return an ID token")
		return
	}
	idToken, err := s.oidc.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "oidc_invalid", "the identity token could not be verified")
		return
	}
	var claims struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
		Nonce         string `json:"nonce"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" || claims.Nonce != attempt.Nonce ||
		claims.EmailVerified == nil || !*claims.EmailVerified {
		writeError(w, r, http.StatusUnauthorized, "oidc_invalid", "a verified email and valid nonce are required")
		return
	}
	var displayName *string
	if strings.TrimSpace(claims.Name) != "" {
		name := strings.TrimSpace(claims.Name)
		displayName = &name
	}
	var user db.User
	var invitedTenant *db.Tenant
	if attempt.InvitationID != nil {
		acceptedUser, tenant, acceptErr := db.AcceptOIDCInvitation(r.Context(), s.database, *attempt.InvitationID,
			attempt.InvitationTokenHash, s.oidc.issuer, claims.Subject, claims.Email, displayName, requestIDFrom(r))
		if errors.Is(acceptErr, db.ErrEmailMismatch) {
			writeError(w, r, http.StatusForbidden, "invitation_email_mismatch", "sign in with the verified email address named by the invitation")
			return
		}
		if errors.Is(acceptErr, sql.ErrNoRows) || invitationUnavailable(acceptErr) {
			writeError(w, r, http.StatusGone, "invitation_unavailable", "the invitation expired, was revoked, or was already used")
			return
		}
		if errors.Is(acceptErr, db.ErrConflict) {
			writeError(w, r, http.StatusConflict, "identity_conflict", "that email belongs to a different identity or is already a member")
			return
		}
		if acceptErr != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "invitation acceptance failed")
			return
		}
		user, invitedTenant = acceptedUser, &tenant
	} else {
		user, err = db.UpsertOIDCUser(r.Context(), s.database, s.oidc.issuer, claims.Subject, claims.Email, displayName)
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "identity_conflict", "that email belongs to a different identity")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "identity creation failed")
		return
	}
	if attempt.CLIReturnURL != nil {
		code, err := db.CreateCLILoginCode(r.Context(), s.database, user, time.Minute)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "CLI login completion failed")
			return
		}
		target, _ := url.Parse(*attempt.CLIReturnURL)
		query := target.Query()
		query.Set("code", code)
		target.RawQuery = query.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
		return
	}
	session, err := db.CreateControlSession(r.Context(), s.database, db.FullPrincipal(user), 12*time.Hour)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "browser session creation failed")
		return
	}
	s.setControlCookie(w, session, 12*time.Hour)
	if invitedTenant != nil {
		http.Redirect(w, r, invitedDashboardPath(invitedTenant.Slug), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) exchangeCLILogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(w, r, &input); err != nil || input.Code == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a one-time login code is required")
		return
	}
	user, secret, token, err := db.ExchangeCLILoginCode(r.Context(), s.database, input.Code, 30*24*time.Hour)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusUnauthorized, "invalid_login_code", "the login code expired or was already used")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "CLI login exchange failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "token": secret, "metadata": token})
}

func validateCLIReturn(value string) error {
	target, err := url.Parse(value)
	if err != nil || target.Scheme != "http" || target.User != nil || target.Fragment != "" || target.Host == "" {
		return fmt.Errorf("CLI return URL must be an absolute loopback HTTP URL")
	}
	host, port, err := net.SplitHostPort(target.Host)
	if err != nil || port == "" || !loopbackHost(host) {
		return fmt.Errorf("CLI return URL must use a loopback host and explicit port")
	}
	return nil
}
