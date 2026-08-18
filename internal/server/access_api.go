package server

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"nebulous/internal/db"
)

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "member listing does not accept query parameters")
		return
	}
	members, err := db.ListMembers(r.Context(), s.database, tenant.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "member listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.adminTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	member, err := db.AddMember(r.Context(), s.database, tenant, actor, input.Email, input.Role, requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "identity_not_found", "that user must sign in once before being added")
		return
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "member_conflict", "the role is not allowed or the user is already a member")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "member creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"member": member})
}

func (s *Server) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("user")) {
		writeError(w, r, http.StatusNotFound, "not_found", "member was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.adminTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	member, err := db.ChangeMemberRole(r.Context(), s.database, tenant, actor, r.PathValue("user"), input.Role, requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "member was not found")
		return
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "role_conflict", "that role change is not allowed or would remove the final owner")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "member role change failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member": member})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("user")) {
		writeError(w, r, http.StatusNotFound, "not_found", "member was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.adminTenant(w, r, actor)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "member removal does not accept query parameters")
		return
	}
	err := db.RemoveMember(r.Context(), s.database, tenant, actor, r.PathValue("user"), requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "member was not found")
		return
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "member_conflict", "that member cannot be removed or is the final owner")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "member removal failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "token listing does not accept query parameters")
		return
	}
	tokens, err := db.ListAPITokens(r.Context(), s.database, actor, tenant.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "token listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresIn int      `json:"expires_in_days"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.ExpiresIn == 0 {
		input.ExpiresIn = 30
	}
	if input.ExpiresIn < 1 || input.ExpiresIn > 365 {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_expiry", "expires_in_days must be from 1 to 365")
		return
	}
	secret, token, err := db.CreateAPIToken(r.Context(), s.database, actor, &tenant.ID, input.Name, input.Scopes,
		time.Now().UTC().Add(time.Duration(input.ExpiresIn)*24*time.Hour), requestIDFrom(r))
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_token", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "secret": secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("token")) {
		writeError(w, r, http.StatusNotFound, "not_found", "token was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "token revocation does not accept query parameters")
		return
	}
	err := db.RevokeAPIToken(r.Context(), s.database, actor, tenant.ID, r.PathValue("token"), requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "token was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "token revocation failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeCurrentToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "token revocation does not accept query parameters")
		return
	}
	principal := r.Context().Value(principalContextKey{}).(db.Principal)
	if principal.TokenID == nil {
		writeError(w, r, http.StatusConflict, "not_api_token", "the current credential is not an API token")
		return
	}
	if err := db.RevokeCurrentAPIToken(r.Context(), s.database, principal.User, *principal.TokenID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "token revocation failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
