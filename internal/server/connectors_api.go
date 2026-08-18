package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"oort/internal/db"
	"oort/internal/secretbox"
)

type connectorRequest struct {
	Slug              string  `json:"slug"`
	Dataset           string  `json:"dataset"`
	URL               string  `json:"url"`
	RecordsPointer    string  `json:"records_pointer"`
	CursorParameter   *string `json:"cursor_parameter,omitempty"`
	NextCursorPointer *string `json:"next_cursor_pointer,omitempty"`
	BearerToken       *string `json:"bearer_token,omitempty"`
	Enabled           *bool   `json:"enabled,omitempty"`
	RefreshMinutes    int     `json:"refresh_minutes"`
}

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	connectors, err := db.ListConnectors(r.Context(), s.database, tenant.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": connectors, "limit": limit, "offset": offset})
}

func (s *Server) getConnector(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	connector, err := db.GetConnector(r.Context(), s.database, tenant.ID, r.PathValue("connector"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "connector was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connector": connector})
}

func (s *Server) createConnector(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	var input connectorRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	dbInput, err := s.connectorInput(input, true)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_connector", err.Error())
		return
	}
	connector, err := db.CreateConnector(r.Context(), s.database, tenant, actor, dbInput, requestIDFrom(r))
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "connector_conflict", "connector or dataset slug is already connected")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"connector": connector})
}

func (s *Server) updateConnector(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	existing, err := db.GetConnector(r.Context(), s.database, tenant.ID, r.PathValue("connector"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "connector was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector lookup failed")
		return
	}
	var input connectorRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input.Slug, input.Dataset = existing.Slug, existing.DatasetSlug
	dbInput, err := s.connectorInput(input, false)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_connector", err.Error())
		return
	}
	connector, err := db.UpdateConnector(r.Context(), s.database, tenant, actor, existing.Slug, dbInput, input.BearerToken != nil, requestIDFrom(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connector": connector})
}

func (s *Server) deleteConnector(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "connector deletion does not accept query parameters")
		return
	}
	err := db.DeleteConnector(r.Context(), s.database, tenant, actor, r.PathValue("connector"), requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "connector was not found")
		return
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "connector_active", "cancel the active connector job before deleting it")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector deletion failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncConnector(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	connector, err := db.GetConnector(r.Context(), s.database, tenant.ID, r.PathValue("connector"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "connector was not found")
		return
	}
	job, err := db.CreateConnectorJob(r.Context(), s.database, tenant, &actor, connector.ID, "manual:"+requestIDFrom(r), requestIDFrom(r))
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "queue_full", "the tenant job queue is full")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "connector sync could not be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) connectorInput(input connectorRequest, create bool) (db.ConnectorInput, error) {
	if create && (!slugPattern.MatchString(input.Slug) || !slugPattern.MatchString(input.Dataset)) {
		return db.ConnectorInput{}, errors.New("connector and dataset slugs must be 3-63 lowercase letters, digits, or hyphens")
	}
	target, err := url.Parse(input.URL)
	if err != nil || target.Host == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "https" && !(s.config.LocalAuth && s.config.AllowPrivateConnectors)) {
		return db.ConnectorInput{}, errors.New("connector URL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if len(input.URL) > 4096 || !validPointer(input.RecordsPointer) ||
		(input.NextCursorPointer != nil && !validPointer(*input.NextCursorPointer)) {
		return db.ConnectorInput{}, errors.New("connector URL or JSON Pointer is invalid")
	}
	if (input.CursorParameter == nil) != (input.NextCursorPointer == nil) ||
		(input.CursorParameter != nil && (strings.TrimSpace(*input.CursorParameter) == "" || len(*input.CursorParameter) > 100)) {
		return db.ConnectorInput{}, errors.New("cursor_parameter and next_cursor_pointer must be configured together")
	}
	if input.RefreshMinutes == 0 {
		input.RefreshMinutes = 60
	}
	if input.RefreshMinutes < 1 || input.RefreshMinutes > 10080 {
		return db.ConnectorInput{}, errors.New("refresh_minutes must be from 1 to 10080")
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	result := db.ConnectorInput{Slug: input.Slug, DatasetSlug: input.Dataset, URL: target.String(),
		RecordsPointer: input.RecordsPointer, CursorParameter: input.CursorParameter,
		NextCursorPointer: input.NextCursorPointer, Enabled: enabled, RefreshMinutes: input.RefreshMinutes}
	if input.BearerToken != nil && *input.BearerToken != "" {
		box, err := secretbox.New(s.config.SecretKey)
		if err != nil {
			return db.ConnectorInput{}, err
		}
		result.Ciphertext, result.Nonce, err = box.Seal(*input.BearerToken)
		if err != nil {
			return db.ConnectorInput{}, err
		}
	}
	return result, nil
}

func validPointer(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") || len(value) > 1000 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] != '~' {
			continue
		}
		index++
		if index == len(value) || (value[index] != '0' && value[index] != '1') {
			return false
		}
	}
	return true
}
