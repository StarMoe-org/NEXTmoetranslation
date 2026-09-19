package api

import (
	"net/http"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
)

// ---- User management (admin only) ----

// handleListUsers returns all users.
//
// GET /api/admin/users
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.auth.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if users == nil {
		users = []auth.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

// handleCreateUser creates a user.
//
// POST /api/admin/users {username, password, role}
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u, err := s.auth.CreateUser(req.Username, req.Password, req.Role)
	if err != nil {
		if err == auth.ErrUserExists {
			writeErr(w, http.StatusConflict, "user already exists")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleUpdateUser changes a user's password and/or role.
//
// PUT /api/admin/users {username, password?, role?}
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "username required")
		return
	}
	if req.Password != "" {
		if err := s.auth.SetPassword(req.Username, req.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.revokeUser(req.Username)
	}
	if req.Role != "" {
		if err := s.auth.SetRole(req.Username, req.Role); err != nil {
			if err == auth.ErrLastAdmin {
				writeErr(w, http.StatusConflict, "cannot demote the last admin")
				return
			}
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.revokeUser(req.Username)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteUser removes a user.
//
// DELETE /api/admin/users?username=
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if username == "" {
		writeErr(w, http.StatusBadRequest, "username required")
		return
	}
	if err := s.auth.DeleteUser(username); err != nil {
		if err == auth.ErrLastAdmin {
			writeErr(w, http.StatusConflict, "cannot delete the last admin")
			return
		}
		if err == auth.ErrUserNotFound {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.revokeUser(username)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleUsersRouter dispatches /api/admin/users by method.
func (s *Server) handleUsersRouter(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListUsers(w, r)
	case http.MethodPost:
		s.handleCreateUser(w, r)
	case http.MethodPut:
		s.handleUpdateUser(w, r)
	case http.MethodDelete:
		s.handleDeleteUser(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---- Settings (admin only) ----

// handleGetSettings returns all settings with secrets masked.
//
// GET /api/admin/settings
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":     s.cfg.All(false),
		"hasMasterKey": s.cfg.HasMasterKey(),
	})
}

// handleUpdateSettings writes one or more settings. Secret values equal to the
// mask sentinel are ignored so the UI can re-submit masked values harmlessly.
//
// PUT /api/admin/settings {key: value, ...}
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if !decodeBody(w, r, &req) {
		return
	}
	patch := make(map[string]string, len(req))
	for k, v := range req {
		if config.IsSecret(k) && v == "********" {
			continue // unchanged masked secret
		}
		patch[k] = v
	}
	applied, err := s.cfg.SetMany(patch)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "applied": applied})
}

// handleSettingsRouter dispatches /api/admin/settings by method.
func (s *Server) handleSettingsRouter(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetSettings(w, r)
	case http.MethodPut:
		s.handleUpdateSettings(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---- Upstream (admin only) ----

// handleUpstreamStatus returns the update-watcher status.
//
// GET /api/admin/upstream
func (s *Server) handleUpstreamStatus(w http.ResponseWriter, r *http.Request) {
	if s.upstream == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.upstream.Status())
}

// handleUpstreamCheck triggers an immediate version check.
//
// POST /api/admin/upstream/check {force?}
func (s *Server) handleUpstreamCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.upstream == nil {
		writeErr(w, http.StatusServiceUnavailable, "upstream watcher not configured")
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	if err := decodeOptional(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	status, err := s.upstream.CheckNowContext(r.Context(), req.Force)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}
