package api

import (
	"net/http"

	"moesekai/server/internal/auth"
)

// handleSetupStatus reports whether first-run setup is needed (no users exist).
// Public: the console shows the registration page instead of login when true.
//
// GET /api/auth/setup-status -> {needsSetup}
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	n, err := s.auth.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to count users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"needsSetup": n == 0})
}

// handleSetup creates the first admin account on a fresh install and logs them
// in. Public, but only succeeds while no users exist; once an account is
// present it returns 409 so the endpoint cannot be used to mint extra admins.
//
// POST /api/auth/setup {username, password} -> {token, username, role, expiresAt}
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !s.allowPublicAuthAttempt(w, r, "setup", req.Username) {
		return
	}
	u, err := s.auth.CreateFirstAdmin(req.Username, req.Password)
	if err != nil {
		if err == auth.ErrSetupComplete || err == auth.ErrUserExists {
			writeErr(w, http.StatusConflict, "setup already completed")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	token, expiresAt, err := s.auth.IssueToken(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"username":  u.Username,
		"role":      u.Role,
		"expiresAt": expiresAt.Unix(),
	})
}

// handleLogin authenticates a user and returns a JWT.
//
// POST /api/auth/login {username, password} -> {token, username, role, expiresAt}
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !s.allowPublicAuthAttempt(w, r, "login", req.Username) {
		return
	}
	u, err := s.auth.Authenticate(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, expiresAt, err := s.auth.IssueToken(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"username":  u.Username,
		"role":      u.Role,
		"expiresAt": expiresAt.Unix(),
	})
}

// handleMe returns the current user from the JWT.
//
// GET /api/auth/me -> {username, role}
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": claims.Username,
		"role":     claims.Role,
	})
}

// handleRefresh issues a fresh token for the current (still-valid) session.
//
// POST /api/auth/refresh -> {token, expiresAt}
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims, ok := auth.FromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, expiresAt, err := s.auth.RefreshToken(claims)
	if err != nil {
		if err == auth.ErrInvalidCreds {
			writeErr(w, http.StatusUnauthorized, "session was revoked")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	s.revokeUser(claims.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"expiresAt": expiresAt.Unix(),
	})
}
