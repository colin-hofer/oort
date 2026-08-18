package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"oort/internal/db"
	"oort/internal/queryexec"
)

type queryInput struct {
	SQL            string            `json:"sql"`
	Parameters     map[string]any    `json:"parameters"`
	ParameterTypes map[string]string `json:"parameter_types,omitempty"`
}

func (s *Server) validateQuery(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	if _, ok := s.developerTenant(w, r, actor); !ok {
		return
	}
	var input queryInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	parameters, err := queryParameters(input)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	cleaned, types, err := queryexec.Validate(input.SQL, parameters)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sql": cleaned, "parameter_types": types})
}

func (s *Server) executeDraftQuery(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	var input queryInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cleaned, types, err := queryexec.Validate(input.SQL, input.Parameters)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	s.writeQueryResult(w, r, tenant, cleaned, input.Parameters, types, 10_000)
}

func (s *Server) saveQuery(w http.ResponseWriter, r *http.Request) {
	if !slugPattern.MatchString(r.PathValue("query")) {
		writeError(w, r, http.StatusNotFound, "not_found", "query was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	var input queryInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	parameters, err := queryParameters(input)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	cleaned, types, err := queryexec.Validate(input.SQL, parameters)
	if err != nil || (input.ParameterTypes != nil && !reflect.DeepEqual(types, input.ParameterTypes)) {
		if err == nil {
			err = fmt.Errorf("declared parameter types do not match the SQL parameters")
		}
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	revision, err := db.SaveQueryRevision(r.Context(), s.database, tenant, actor, r.PathValue("query"), cleaned, types, requestIDFrom(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "saving the query revision failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": revision})
}

func (s *Server) executeSavedQuery(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	revision, err := db.GetCurrentQuery(r.Context(), s.database, tenant.ID, r.PathValue("query"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "query was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "query lookup failed")
		return
	}
	cleaned, types, err := queryexec.Validate(revision.SQL, input.Parameters)
	if err != nil || !reflect.DeepEqual(types, revision.ParameterTypes) {
		if err == nil {
			err = fmt.Errorf("query parameter types do not match the saved revision")
		}
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_parameters", err.Error())
		return
	}
	s.writeQueryResultWith(w, r, tenant, cleaned, input.Parameters, types, 10_000, map[string]any{"query": revision})
}

func (s *Server) listQueries(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	queries, err := db.ListQueries(r.Context(), s.database, tenant.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "query listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queries": queries, "limit": limit, "offset": offset})
}

func (s *Server) getQuery(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	revisions, err := db.ListQueryRevisions(r.Context(), s.database, tenant.ID, r.PathValue("query"), 100, 0)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "query lookup failed")
		return
	}
	if len(revisions) == 0 {
		writeError(w, r, http.StatusNotFound, "not_found", "query was not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": revisions[0], "revisions": revisions})
}

func queryParameters(input queryInput) (map[string]any, error) {
	if input.ParameterTypes == nil {
		if input.Parameters == nil {
			return map[string]any{}, nil
		}
		return input.Parameters, nil
	}
	if len(input.Parameters) > 0 {
		return nil, fmt.Errorf("provide parameters or parameter_types, not both")
	}
	parameters := make(map[string]any, len(input.ParameterTypes))
	for name, kind := range input.ParameterTypes {
		switch kind {
		case "boolean":
			parameters[name] = false
		case "integer":
			parameters[name] = float64(0)
		case "number":
			parameters[name] = 0.5
		case "string":
			parameters[name] = ""
		default:
			return nil, fmt.Errorf("parameter %q has unsupported type %q", name, kind)
		}
	}
	return parameters, nil
}

func (s *Server) writeQueryResult(w http.ResponseWriter, r *http.Request, tenant db.Tenant, sqlText string, parameters map[string]any, types map[string]string, maxRows int) {
	s.writeQueryResultWith(w, r, tenant, sqlText, parameters, types, maxRows, nil)
}

func (s *Server) writeQueryResultWith(w http.ResponseWriter, r *http.Request, tenant db.Tenant, sqlText string, parameters map[string]any, types map[string]string, maxRows int, extra map[string]any) {
	result, err := s.executeSQL(r.Context(), tenant, sqlText, parameters, types, maxRows)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, r, http.StatusGatewayTimeout, "query_timeout", "query exceeded the 10-second limit")
			return
		}
		writeError(w, r, http.StatusUnprocessableEntity, "query_failed", err.Error())
		return
	}
	response := map[string]any{"result": json.RawMessage(result)}
	for key, value := range extra {
		response[key] = value
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) executeSQL(ctx context.Context, tenant db.Tenant, sqlText string, parameters map[string]any, types map[string]string, maxRows int) ([]byte, error) {
	if s.objects == nil || s.executable == "" {
		return nil, fmt.Errorf("query execution is not configured")
	}
	s.queryMu.Lock()
	if s.querySlots == nil {
		s.querySlots = map[string]chan struct{}{}
	}
	slots := s.querySlots[tenant.ID]
	if slots == nil {
		slots = make(chan struct{}, 2)
		s.querySlots[tenant.ID] = slots
	}
	s.queryMu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	catalogURL, _, _, err := db.TenantCatalog(s.config.DatabaseURL, s.config.CatalogSecret, tenant.ID)
	if err != nil {
		return nil, err
	}
	timeout := s.config.QueryTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	queryContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result bytes.Buffer
	err = queryexec.Run(queryContext, s.executable, queryexec.Request{
		CatalogURL: catalogURL, DataPath: s.objects.DataPath(tenant.ID), ExtensionDir: s.config.ExtensionDir,
		Storage: s.config.Storage, SQL: sqlText, Parameters: parameters, ParameterTypes: types,
		MaxRows: maxRows, MaxBytes: 10 << 20,
	}, &result)
	if err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

func requestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestContextKey{}).(string)
	return id
}
