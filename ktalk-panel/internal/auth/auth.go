// Package auth implements admin authentication for ktalk-panel.
//
// First-run: GET /admin → redirect to /admin/setup.
// Setup: POST /api/auth/setup with {"password": "..."} — stores bcrypt hash.
// Login: POST /api/auth/login with {"password": "..."} — sets cookie.
// Auth: requests to /api/* require the session cookie (or Bearer token).
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName    = "ktalk_session"
	sessionTTL    = 24 * time.Hour
	bcryptCost    = 12
	tokenLen      = 32
	maxLoginFails = 10
	lockoutPeriod = 5 * time.Minute
)

// SessionStore manages active admin sessions.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
}

// NewSessionStore creates an empty session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]time.Time)}
}

// Create creates a new session token valid for sessionTTL.
func (s *SessionStore) Create() (string, error) {
	b := make([]byte, tokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return tok, nil
}

// Valid returns true if the token is present and not expired.
func (s *SessionStore) Valid(tok string) bool {
	s.mu.RLock()
	exp, ok := s.sessions[tok]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		s.mu.Lock()
		delete(s.sessions, tok)
		s.mu.Unlock()
		return false
	}
	return true
}

// Delete invalidates a session.
func (s *SessionStore) Delete(tok string) {
	s.mu.Lock()
	delete(s.sessions, tok)
	s.mu.Unlock()
}

// Limiter prevents brute-force login attempts.
type Limiter struct {
	mu      sync.Mutex
	fails   map[string]int
	lockout map[string]time.Time
}

// NewLimiter creates a new rate limiter.
func NewLimiter() *Limiter {
	return &Limiter{
		fails:   make(map[string]int),
		lockout: make(map[string]time.Time),
	}
}

// Allow returns true if the IP is not locked out.
func (l *Limiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if exp, ok := l.lockout[ip]; ok {
		if time.Now().Before(exp) {
			return false
		}
		delete(l.lockout, ip)
		delete(l.fails, ip)
	}
	return true
}

// RecordFail records a failed login attempt.
func (l *Limiter) RecordFail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip]++
	if l.fails[ip] >= maxLoginFails {
		l.lockout[ip] = time.Now().Add(lockoutPeriod)
	}
}

// RecordSuccess resets the fail counter.
func (l *Limiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
	delete(l.lockout, ip)
}

// HashPassword returns a bcrypt hash of the given password.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword returns true if password matches the stored bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Middleware wraps an HTTP handler and requires a valid session cookie.
func Middleware(sessions *SessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := sessionToken(r)
		if !sessions.Valid(tok) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSessionCookie attaches a secure HTTP-only session cookie to the response.
func SetSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSessionCookie removes the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	// Also check Bearer token for API clients
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	return ""
}
