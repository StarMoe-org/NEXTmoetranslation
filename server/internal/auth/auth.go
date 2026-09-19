// Package auth provides user management (bcrypt-hashed passwords), JWT issuance
// and verification, role-based access control (admin/editor), and HTTP
// middleware. It replaces the legacy "user:secret" pseudo-token scheme.
package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"moesekai/server/internal/db"
)

// Roles.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
)

func ValidRole(r string) bool { return r == RoleAdmin || r == RoleEditor }

// User is a console account.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	CreatedAt    int64  `json:"createdAt"`
	TokenVersion int    `json:"-"`
}

var (
	ErrUserExists    = errors.New("user already exists")
	ErrUserNotFound  = errors.New("user not found")
	ErrInvalidCreds  = errors.New("invalid credentials")
	ErrLastAdmin     = errors.New("cannot remove the last admin")
	ErrSetupComplete = errors.New("setup already completed")
	ErrInvalidRole   = errors.New("role must be editor or admin")
	ErrWeakPassword  = errors.New("password must be 12-72 bytes and at least 12 characters")
	ErrWeakJWTSecret = errors.New("JWT secret must contain at least 32 bytes")
)

var refreshTokenValidatedHook func()

// Auth manages users and tokens.
type Auth struct {
	db        *db.DB
	jwtSecret []byte
	tokenTTL  time.Duration
}

func New(database *db.DB, jwtSecret string, ttl time.Duration) *Auth {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Auth{db: database, jwtSecret: []byte(jwtSecret), tokenTTL: ttl}
}

func ValidateJWTSecret(secret string) error {
	if len([]byte(secret)) < 32 || strings.TrimSpace(secret) == "replace-with-at-least-32-random-bytes" {
		return ErrWeakJWTSecret
	}
	return nil
}

func validatePassword(password string) error {
	if len([]byte(password)) < 12 || len([]byte(password)) > 72 || utf8.RuneCountInString(password) < 12 {
		return ErrWeakPassword
	}
	return nil
}

// ---- User CRUD ----

// CreateUser adds a user with a bcrypt-hashed password.
func (a *Auth) CreateUser(username, password, role string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("username and password required")
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	if !ValidRole(role) {
		return nil, ErrInvalidRole
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := a.db.Exec(
		`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		username, string(hash), role, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Role: role, CreatedAt: now, TokenVersion: 1}, nil
}

// CreateFirstAdmin atomically verifies that no account exists and creates the
// initial administrator. The immediate SQLite transaction serializes concurrent
// setup attempts from different processes or requests.
func (a *Auth) CreateFirstAdmin(username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("username and password required")
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return nil, err
	}
	if count != 0 {
		return nil, ErrSetupComplete
	}
	now := time.Now().Unix()
	result, err := tx.Exec(`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		username, string(hash), RoleAdmin, now)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, Role: RoleAdmin, CreatedAt: now, TokenVersion: 1}, nil
}

// ListUsers returns all users ordered by id (no password hashes).
func (a *Auth) ListUsers() ([]User, error) {
	rows, err := a.db.Query(`SELECT id, username, role, created_at, token_version FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.TokenVersion); err != nil {
			return nil, err
		}
		if !ValidRole(u.Role) {
			return nil, fmt.Errorf("invalid persisted role for user %q: %w", u.Username, ErrInvalidRole)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword updates a user's password.
func (a *Auth) SetPassword(username, password string) error {
	if password == "" {
		return errors.New("password required")
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`UPDATE users SET password_hash = ?, token_version = token_version + 1 WHERE username = ?`, string(hash), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetRole updates a user's role, refusing to demote the last admin.
func (a *Auth) SetRole(username, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("invalid role: %s", role)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role != RoleAdmin {
		if err := guardLastAdmin(tx, username); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE users SET role = ?, token_version = token_version + 1 WHERE username = ?`, role, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return tx.Commit()
}

// DeleteUser removes a user, refusing to remove the last admin.
func (a *Auth) DeleteUser(username string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := guardLastAdmin(tx, username); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return tx.Commit()
}

// guardLastAdmin returns ErrLastAdmin if username is currently the only admin.
// The caller holds the same immediate transaction used for the mutation.
func guardLastAdmin(tx *sql.Tx, username string) error {
	var role string
	err := tx.QueryRow(`SELECT role FROM users WHERE username = ?`, username).Scan(&role)
	if err == sql.ErrNoRows {
		return nil // not a user; nothing to guard
	}
	if err != nil {
		return err
	}
	if role != RoleAdmin {
		return nil
	}
	var adminCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&adminCount); err != nil {
		return err
	}
	if adminCount <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// CountUsers returns the total number of users (used for first-run seeding).
func (a *Auth) CountUsers() (int, error) {
	var n int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (a *Auth) CountAdmins() (int, error) {
	var n int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role=?`, RoleAdmin).Scan(&n)
	return n, err
}

// ValidatePersistedRoles rejects databases containing principals outside the
// closed editor/admin role vocabulary before startup can seed or serve users.
func (a *Auth) ValidatePersistedRoles() error {
	var username, role string
	err := a.db.QueryRow(`SELECT username, role FROM users WHERE role NOT IN (?, ?) ORDER BY id LIMIT 1`,
		RoleEditor, RoleAdmin).Scan(&username, &role)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("user %q has invalid persisted role %q: %w", username, role, ErrInvalidRole)
}

// ---- Authentication ----

// Authenticate verifies credentials and returns the user.
func (a *Auth) Authenticate(username, password string) (*User, error) {
	var u User
	var hash string
	err := a.db.QueryRow(
		`SELECT id, username, password_hash, role, created_at, token_version FROM users WHERE username = ?`,
		strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.CreatedAt, &u.TokenVersion)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidCreds
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrInvalidCreds
	}
	if !ValidRole(u.Role) {
		return nil, ErrInvalidCreds
	}
	return &u, nil
}

// GetUser returns a user by username (no password hash).
func (a *Auth) GetUser(username string) (*User, error) {
	var u User
	err := a.db.QueryRow(
		`SELECT id, username, role, created_at, token_version FROM users WHERE username = ?`,
		username).Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.TokenVersion)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	if !ValidRole(u.Role) {
		return nil, fmt.Errorf("invalid persisted role for user %q: %w", u.Username, ErrInvalidRole)
	}
	return &u, nil
}

// ---- JWT ----

type Claims struct {
	UserID       int64  `json:"uid"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	TokenVersion int    `json:"ver"`
	jwt.RegisteredClaims
}

// IssueToken creates a signed JWT for the user.
func (a *Auth) IssueToken(u *User) (string, time.Time, error) {
	if u == nil || !ValidRole(u.Role) {
		return "", time.Time{}, ErrInvalidRole
	}
	return a.issueToken(u.ID, u.Username, u.Role, u.TokenVersion)
}

func (a *Auth) issueToken(userID int64, username, role string, tokenVersion int) (string, time.Time, error) {
	if !ValidRole(role) {
		return "", time.Time{}, ErrInvalidRole
	}
	if err := ValidateJWTSecret(string(a.jwtSecret)); err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(a.tokenTTL)
	claims := Claims{
		UserID:       userID,
		Username:     username,
		Role:         role,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(a.jwtSecret)
	return signed, expiresAt, err
}

// RefreshToken atomically consumes the authenticated generation and signs its
// successor. A replay or concurrent refresh cannot reuse the consumed version,
// and a concurrent revocation either rejects refresh or invalidates its result.
func (a *Auth) RefreshToken(claims *Claims) (string, time.Time, error) {
	if claims == nil || !ValidRole(claims.Role) {
		return "", time.Time{}, ErrInvalidCreds
	}
	tx, err := a.db.Begin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE users SET token_version = token_version + 1
		WHERE id = ? AND username = ? AND role = ? AND token_version = ?`,
		claims.UserID, claims.Username, claims.Role, claims.TokenVersion)
	if err != nil {
		return "", time.Time{}, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return "", time.Time{}, err
	} else if changed != 1 {
		return "", time.Time{}, ErrInvalidCreds
	}
	if refreshTokenValidatedHook != nil {
		refreshTokenValidatedHook()
	}
	token, expiresAt, err := a.issueToken(claims.UserID, claims.Username, claims.Role, claims.TokenVersion+1)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// VerifyToken parses and validates a JWT, returning its claims.
func (a *Auth) VerifyToken(tokenStr string) (*Claims, error) {
	if ValidateJWTSecret(string(a.jwtSecret)) != nil {
		return nil, ErrInvalidCreds
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return a.jwtSecret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || !token.Valid {
		return nil, ErrInvalidCreds
	}
	if !ValidRole(claims.Role) {
		return nil, ErrInvalidCreds
	}
	var currentUsername, currentRole string
	var currentVersion int
	// Look the row up by id: it is AUTOINCREMENT and therefore never reused, so
	// recreating a deleted username cannot revive tokens issued to the old account.
	if err := a.db.QueryRow(`SELECT username, role, token_version FROM users WHERE id=?`, claims.UserID).
		Scan(&currentUsername, &currentRole, &currentVersion); err != nil || !ValidRole(currentRole) ||
		currentUsername != claims.Username || currentRole != claims.Role || currentVersion != claims.TokenVersion {
		return nil, ErrInvalidCreds
	}
	return claims, nil
}
