package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"moesekai/server/internal/db"
)

func TestStrongSecretsAndHS256Only(t *testing.T) {
	a := openTestAuth(t)
	if _, err := a.CreateUser("weak", "short", RoleEditor); err != ErrWeakPassword {
		t.Fatalf("weak password error = %v", err)
	}
	user, err := a.CreateUser("editor", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{Subject: user.Username, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString(a.jwtSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(token); err != ErrInvalidCreds {
		t.Fatalf("HS384 token error = %v", err)
	}
	weak := New(a.db, "too-short", time.Hour)
	if _, _, err := weak.IssueToken(user); err != ErrWeakJWTSecret {
		t.Fatalf("weak JWT secret error = %v", err)
	}
}

func openTestAuth(t *testing.T) *Auth {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database, "jwt-secret-at-least-32-bytes-long", time.Hour)
}

func TestValidateJWTSecretRejectsPublishedTemplate(t *testing.T) {
	if err := ValidateJWTSecret("replace-with-at-least-32-random-bytes"); err != ErrWeakJWTSecret {
		t.Fatalf("template JWT secret error = %v", err)
	}
	if err := ValidateJWTSecret("jwt-secret-at-least-32-bytes-long"); err != nil {
		t.Fatalf("ordinary test JWT secret rejected: %v", err)
	}
}

func TestCreateAndAuthenticate(t *testing.T) {
	a := openTestAuth(t)
	if _, err := a.CreateUser("alice", "strong-password-123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	u, err := a.Authenticate("alice", "strong-password-123")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("role: got %q", u.Role)
	}
	if _, err := a.Authenticate("alice", "wrong"); err != ErrInvalidCreds {
		t.Errorf("expected ErrInvalidCreds, got %v", err)
	}
}

func TestUnknownRolesAreRejectedAtEveryAuthenticationBoundary(t *testing.T) {
	a := openTestAuth(t)
	if _, err := a.CreateUser("viewer", "strong-password-123", "viewer"); err != ErrInvalidRole {
		t.Fatalf("create viewer error = %v", err)
	}
	user, err := a.CreateUser("injected", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE users SET role='viewer' WHERE username=?`, user.Username); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(user.Username, "strong-password-123"); err != ErrInvalidCreds {
		t.Fatalf("viewer login error = %v", err)
	}
	if _, err := a.GetUser(user.Username); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("get viewer error = %v", err)
	}
	if err := a.ValidatePersistedRoles(); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("persisted viewer validation error = %v", err)
	}
	if _, _, err := a.IssueToken(&User{Username: user.Username, Role: "viewer", TokenVersion: 1}); err != ErrInvalidRole {
		t.Fatalf("issue viewer token error = %v", err)
	}
	claims := Claims{Username: user.Username, Role: "viewer", TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{Subject: user.Username, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.jwtSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(token); err != ErrInvalidCreds {
		t.Fatalf("verify viewer token error = %v", err)
	}
}

func TestDuplicateUser(t *testing.T) {
	a := openTestAuth(t)
	a.CreateUser("bob", "strong-password-123", RoleEditor)
	if _, err := a.CreateUser("bob", "strong-password-456", RoleEditor); err != ErrUserExists {
		t.Errorf("expected ErrUserExists, got %v", err)
	}
}

func TestJWTRoundTrip(t *testing.T) {
	a := openTestAuth(t)
	u, _ := a.CreateUser("carol", "strong-password-123", RoleEditor)
	token, _, err := a.IssueToken(u)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.VerifyToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Username != "carol" || claims.Role != RoleEditor {
		t.Errorf("claims mismatch: %+v", claims)
	}
	if _, err := a.VerifyToken("garbage.token.here"); err == nil {
		t.Error("expected error for garbage token")
	}
}

func TestRequireAuthAcceptsOnlyBearerHeader(t *testing.T) {
	a := openTestAuth(t)
	user, err := a.CreateUser("bearer", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueToken(user)
	if err != nil {
		t.Fatal(err)
	}
	handler := a.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := FromContext(r.Context())
		if !ok || claims.Username != user.Username {
			t.Fatalf("claims = %+v, ok = %v", claims, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	queryOnly := httptest.NewRequest(http.MethodGet, "/sse?token="+token, nil)
	queryResponse := httptest.NewRecorder()
	handler(queryResponse, queryOnly)
	if queryResponse.Code != http.StatusUnauthorized {
		t.Fatalf("query token status = %d", queryResponse.Code)
	}

	bearer := httptest.NewRequest(http.MethodGet, "/sse", nil)
	bearer.Header.Set("Authorization", "Bearer "+token)
	bearerResponse := httptest.NewRecorder()
	handler(bearerResponse, bearer)
	if bearerResponse.Code != http.StatusNoContent {
		t.Fatalf("bearer status = %d", bearerResponse.Code)
	}
}

func TestExpiredToken(t *testing.T) {
	database, _ := db.Open(t.TempDir() + "/exp.db")
	defer database.Close()
	a := New(database, "jwt-secret-at-least-32-bytes-long", time.Hour)
	a.tokenTTL = -time.Hour // force already-expired tokens (bypasses New's clamp)
	u, _ := a.CreateUser("dave", "strong-password-123", RoleEditor)
	token, _, _ := a.IssueToken(u)
	if _, err := a.VerifyToken(token); err == nil {
		t.Error("expected expired token to fail verification")
	}
}

func TestLastAdminGuard(t *testing.T) {
	a := openTestAuth(t)
	a.CreateUser("admin1", "strong-password-123", RoleAdmin)
	a.CreateUser("editor1", "strong-password-123", RoleEditor)

	// Demoting the only admin must fail.
	if err := a.SetRole("admin1", RoleEditor); err != ErrLastAdmin {
		t.Errorf("expected ErrLastAdmin on demote, got %v", err)
	}
	// Deleting the only admin must fail.
	if err := a.DeleteUser("admin1"); err != ErrLastAdmin {
		t.Errorf("expected ErrLastAdmin on delete, got %v", err)
	}
	// With a second admin, demotion is allowed.
	a.CreateUser("admin2", "strong-password-456", RoleAdmin)
	if err := a.SetRole("admin1", RoleEditor); err != nil {
		t.Errorf("demote with 2 admins should succeed: %v", err)
	}
}

func TestWrongSecretRejected(t *testing.T) {
	a := openTestAuth(t)
	u, _ := a.CreateUser("eve", "strong-password-123", RoleEditor)
	token, _, _ := a.IssueToken(u)
	other := New(a.db, "different-secret", time.Hour)
	if _, err := other.VerifyToken(token); err == nil {
		t.Error("token signed with one secret must not verify under another")
	}
}

func TestCreateFirstAdminIsAtomicUnderConcurrency(t *testing.T) {
	a := openTestAuth(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, username := range []string{"first", "second"} {
		wait.Add(1)
		go func(username string) {
			defer wait.Done()
			<-start
			_, err := a.CreateFirstAdmin(username, "strong-password-123")
			results <- err
		}(username)
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, completed := 0, 0
	for err := range results {
		switch err {
		case nil:
			succeeded++
		case ErrSetupComplete:
			completed++
		default:
			t.Fatalf("concurrent setup error = %v", err)
		}
	}
	count, err := a.CountUsers()
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || completed != 1 || count != 1 {
		t.Fatalf("setup success=%d complete=%d users=%d", succeeded, completed, count)
	}
}

func TestRolePasswordAndDeleteRevokeIssuedTokens(t *testing.T) {
	a := openTestAuth(t)
	admin, err := a.CreateUser("admin", "old-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser("backup-admin", "strong-password-456", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	roleToken, _, err := a.IssueToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetRole("admin", RoleEditor); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(roleToken); err != ErrInvalidCreds {
		t.Fatalf("demoted admin token error = %v", err)
	}

	current, err := a.Authenticate("admin", "old-password")
	if err != nil || current.Role != RoleEditor {
		t.Fatalf("current demoted user = %+v err=%v", current, err)
	}
	passwordToken, _, err := a.IssueToken(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPassword("admin", "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(passwordToken); err != ErrInvalidCreds {
		t.Fatalf("password-reset token error = %v", err)
	}
	if _, err := a.Authenticate("admin", "old-password"); err != ErrInvalidCreds {
		t.Fatalf("old password error = %v", err)
	}

	if err := a.SetRole("admin", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	current, err = a.Authenticate("admin", "new-password")
	if err != nil {
		t.Fatal(err)
	}
	deleteToken, _, err := a.IssueToken(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser("admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(deleteToken); err != ErrInvalidCreds {
		t.Fatalf("deleted admin token error = %v", err)
	}
}

func TestRecreatingAUsernameDoesNotRestoreTheDeletedAccountTokens(t *testing.T) {
	a := openTestAuth(t)
	deleted, err := a.CreateUser("editor", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueToken(deleted)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser("editor"); err != nil {
		t.Fatal(err)
	}
	replacement, err := a.CreateUser("editor", "another-strong-password", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == deleted.ID {
		t.Fatalf("recreated account reused id %d", replacement.ID)
	}
	if _, err := a.VerifyToken(token); err != ErrInvalidCreds {
		t.Fatalf("token of the deleted account error = %v", err)
	}
}

func TestRefreshTokenCannotBeReplayed(t *testing.T) {
	a := openTestAuth(t)
	user, err := a.CreateUser("editor", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueToken(user)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	replacement, _, err := a.RefreshToken(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(token); err != ErrInvalidCreds {
		t.Fatalf("consumed token verification error = %v", err)
	}
	if _, _, err := a.RefreshToken(claims); err != ErrInvalidCreds {
		t.Fatalf("sequential refresh replay error = %v", err)
	}
	replacementClaims, err := a.VerifyToken(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if replacementClaims.TokenVersion != claims.TokenVersion+1 {
		t.Fatalf("replacement version = %d, want %d", replacementClaims.TokenVersion, claims.TokenVersion+1)
	}
}

func TestConcurrentRefreshTokenDoubleSpendAllowsOneReplacement(t *testing.T) {
	a := openTestAuth(t)
	user, err := a.CreateUser("editor", "strong-password-123", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueToken(user)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		token string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			replacement, _, err := a.RefreshToken(claims)
			results <- result{token: replacement, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, rejected := 0, 0
	var replacement string
	for result := range results {
		switch result.err {
		case nil:
			succeeded++
			replacement = result.token
		case ErrInvalidCreds:
			rejected++
		default:
			t.Fatalf("concurrent refresh error = %v", result.err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent refresh success=%d rejected=%d", succeeded, rejected)
	}
	replacementClaims, err := a.VerifyToken(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if replacementClaims.TokenVersion != claims.TokenVersion+1 {
		t.Fatalf("replacement version = %d, want %d", replacementClaims.TokenVersion, claims.TokenVersion+1)
	}
}

func TestRefreshTokenIsAtomicWithRevocation(t *testing.T) {
	a := openTestAuth(t)
	admin, err := a.CreateUser("admin", "strong-password-123", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser("backup-admin", "strong-password-456", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	validated := make(chan struct{})
	release := make(chan struct{})
	refreshTokenValidatedHook = func() {
		close(validated)
		<-release
	}
	t.Cleanup(func() { refreshTokenValidatedHook = nil })
	type refreshResult struct {
		token string
		err   error
	}
	refreshed := make(chan refreshResult, 1)
	go func() {
		newToken, _, err := a.RefreshToken(claims)
		refreshed <- refreshResult{token: newToken, err: err}
	}()
	<-validated
	revoked := make(chan error, 1)
	go func() { revoked <- a.SetRole("admin", RoleEditor) }()
	select {
	case err := <-revoked:
		t.Fatalf("revocation crossed active refresh transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	result := <-refreshed
	if result.err != nil {
		t.Fatal(result.err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyToken(result.token); err != ErrInvalidCreds {
		t.Fatalf("refreshed pre-revocation generation error = %v", err)
	}

	current, err := a.Authenticate("admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	currentToken, _, err := a.IssueToken(current)
	if err != nil {
		t.Fatal(err)
	}
	staleClaims, err := a.VerifyToken(currentToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPassword("admin", "new-password"); err != nil {
		t.Fatal(err)
	}
	refreshTokenValidatedHook = nil
	if _, _, err := a.RefreshToken(staleClaims); err != ErrInvalidCreds {
		t.Fatalf("refresh after prior revocation error = %v", err)
	}
}

func TestConcurrentAdminMutationsCannotRemoveEveryAdmin(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Auth, string) error
	}{
		{name: "demote", mutate: func(a *Auth, username string) error { return a.SetRole(username, RoleEditor) }},
		{name: "delete", mutate: func(a *Auth, username string) error { return a.DeleteUser(username) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := openTestAuth(t)
			for _, username := range []string{"admin-a", "admin-b"} {
				if _, err := a.CreateUser(username, "strong-password-123", RoleAdmin); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wait sync.WaitGroup
			for _, username := range []string{"admin-a", "admin-b"} {
				wait.Add(1)
				go func(username string) {
					defer wait.Done()
					<-start
					results <- test.mutate(a, username)
				}(username)
			}
			close(start)
			wait.Wait()
			close(results)
			succeeded, protected := 0, 0
			for err := range results {
				switch err {
				case nil:
					succeeded++
				case ErrLastAdmin:
					protected++
				default:
					t.Fatalf("concurrent %s error = %v", test.name, err)
				}
			}
			var adminCount int
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role=?`, RoleAdmin).Scan(&adminCount); err != nil {
				t.Fatal(err)
			}
			if succeeded != 1 || protected != 1 || adminCount != 1 {
				t.Fatalf("%s success=%d protected=%d admins=%d", test.name, succeeded, protected, adminCount)
			}
		})
	}
}
