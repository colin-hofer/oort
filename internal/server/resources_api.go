package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"oort/internal/db"
	"oort/internal/queryexec"
)

func (s *Server) listDatasets(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	datasets, err := db.ListDatasets(r.Context(), s.database, tenant.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets, "limit": limit, "offset": offset})
}

func (s *Server) getDataset(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	dataset, err := db.GetDataset(r.Context(), s.database, tenant.ID, r.PathValue("dataset"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "dataset was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset lookup failed")
		return
	}
	syncs, err := db.ListDatasetSyncs(r.Context(), s.database, tenant.ID, dataset.ID, 50, 0)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset history lookup failed")
		return
	}
	connectors, _ := db.ListConnectors(r.Context(), s.database, tenant.ID, 100, 0)
	var lineage *db.Connector
	for index := range connectors {
		if connectors[index].DatasetID == dataset.ID {
			lineage = &connectors[index]
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dataset": dataset, "syncs": syncs, "connector": lineage})
}

func (s *Server) listDatasetSyncs(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	dataset, err := db.GetDataset(r.Context(), s.database, tenant.ID, r.PathValue("dataset"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "dataset was not found")
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	syncs, err := db.ListDatasetSyncs(r.Context(), s.database, tenant.ID, dataset.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset history lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"syncs": syncs, "limit": limit, "offset": offset})
}

func (s *Server) sampleDataset(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "dataset sampling does not accept query parameters")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	dataset, err := db.GetDataset(r.Context(), s.database, tenant.ID, r.PathValue("dataset"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "dataset was not found")
		return
	}
	if dataset.CurrentSnapshotID == nil {
		writeError(w, r, http.StatusConflict, "dataset_unavailable", "dataset does not have a published snapshot")
		return
	}
	sqlText := `SELECT * FROM "` + strings.ReplaceAll(dataset.Slug, `"`, `""`) + `" LIMIT 100`
	cleaned, types, _ := queryexec.Validate(sqlText, map[string]any{})
	s.writeQueryResult(w, r, tenant, cleaned, map[string]any{}, types, 100)
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	apps, err := db.ListApps(r.Context(), s.database, tenant.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "app listing failed")
		return
	}
	for index := range apps {
		apps[index].URL = s.appURL(apps[index].Slug, tenant.Slug, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps, "limit": limit, "offset": offset})
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	apps, err := db.ListApps(r.Context(), s.database, tenant.ID, 100, 0)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "app lookup failed")
		return
	}
	for _, app := range apps {
		if app.Slug != r.PathValue("app") {
			continue
		}
		app.URL = s.appURL(app.Slug, tenant.Slug, "")
		deployments, err := db.ListDeployments(r.Context(), s.database, tenant.ID, app.Slug, 100, 0)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "app deployment history failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"app": app, "deployments": deployments})
		return
	}
	writeError(w, r, http.StatusNotFound, "not_found", "app was not found")
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	app := r.URL.Query().Get("app")
	limit, offset, ok := page(w, r, "app")
	if !ok {
		return
	}
	deployments, err := db.ListDeployments(r.Context(), s.database, tenant.ID, app, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": deployments, "limit": limit, "offset": offset})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	jobs, err := db.ListJobs(r.Context(), s.database, tenant.ID, limit, offset)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "job listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "limit": limit, "offset": offset})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("job")) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	job, err := db.GetJob(r.Context(), s.database, tenant.ID, r.PathValue("job"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "job lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("job")) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	if _, err := db.GetJob(r.Context(), s.database, tenant.ID, r.PathValue("job")); errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_page", "after must be a non-negative integer")
			return
		}
		after = parsed
	}
	for key := range r.URL.Query() {
		if key != "after" {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "only after is accepted")
			return
		}
	}
	logs, err := db.ListJobLogs(r.Context(), s.database, tenant.ID, r.PathValue("job"), after, 100)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "job logs lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("job")) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	job, err := db.CancelJob(r.Context(), s.database, tenant, actor, r.PathValue("job"), requestIDFrom(r))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "job was not found")
		return
	}
	if errors.Is(err, db.ErrConflict) {
		writeError(w, r, http.StatusConflict, "job_finished", "completed jobs cannot be cancelled")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "job cancellation failed")
		return
	}
	_ = db.AppendJobLog(r.Context(), s.database, tenant.ID, job.ID, "info", "Cancellation requested")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func page(w http.ResponseWriter, r *http.Request, allowed ...string) (int, int, bool) {
	limit, offset := 50, 0
	extra := map[string]bool{}
	for _, key := range allowed {
		extra[key] = true
	}
	for key := range r.URL.Query() {
		if key != "limit" && key != "offset" && !extra[key] {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "only limit and offset are accepted")
			return 0, 0, false
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, r, http.StatusBadRequest, "invalid_page", "limit must be from 1 to 100")
			return 0, 0, false
		}
		limit = parsed
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_page", "offset must be non-negative")
			return 0, 0, false
		}
		offset = parsed
	}
	return limit, offset, true
}
