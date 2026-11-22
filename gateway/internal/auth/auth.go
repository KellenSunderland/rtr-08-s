// Package auth implements bearer-token auth, per-token quotas and the
// internal-endpoint guard.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Principal is an authenticated bearer-token holder together with its quota limits and
// current usage.
type Principal struct {
	Name        string
	MaxStreams  int
	MaxSessions int

	mu       sync.Mutex
	streams  int
	sessions int
}

// Kind identifies which per-token quota is being acquired or released.
type Kind int

const (
	Streams Kind = iota
	Sessions
)

// Acquire reserves one unit of quota k. It returns false, without reserving anything, when the
// limit has already been reached (a limit <= 0 means unlimited).
func (p *Principal) Acquire(k Kind) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch k {
	case Streams:
		if p.MaxStreams > 0 && p.streams >= p.MaxStreams {
			return false
		}
		p.streams++
	default:
		if p.MaxSessions > 0 && p.sessions >= p.MaxSessions {
			return false
		}
		p.sessions++
	}
	return true
}

// Release gives back one unit of quota k. A counter never drops below zero.
func (p *Principal) Release(k Kind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k == Streams && p.streams > 0 {
		p.streams--
	} else if k == Sessions && p.sessions > 0 {
		p.sessions--
	}
}

// Usage returns the number of streams and sessions currently held by this principal.
func (p *Principal) Usage() (streams, sessions int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams, p.sessions
}

type entry struct {
	hash [32]byte
	p    *Principal
}

// Authenticator validates tokens.
type Authenticator struct {
	entries      []entry
	internalHash [32]byte
}

// TokenSpec configures one token.
type TokenSpec struct {
	Name, Token             string
	MaxStreams, MaxSessions int
}

// New builds an Authenticator.
func New(tokens []TokenSpec, internalToken string) *Authenticator {
	a := &Authenticator{internalHash: sha256.Sum256([]byte(internalToken))}
	for _, t := range tokens {
		a.entries = append(a.entries, entry{sha256.Sum256([]byte(t.Token)), &Principal{Name: t.Name, MaxStreams: t.MaxStreams, MaxSessions: t.MaxSessions}})
	}
	return a
}

// Lookup compares the token against every configured token in constant time
// (hashing first equalizes lengths; no early exit).
func (a *Authenticator) Lookup(token string) *Principal {
	h := sha256.Sum256([]byte(token))
	var found *Principal
	for _, e := range a.entries {
		if subtle.ConstantTimeCompare(h[:], e.hash[:]) == 1 {
			found = e.p
		}
	}
	return found
}

// BearerToken extracts the token from the Authorization header. For
// browser WebSocket clients (which cannot set headers) a `access_token`
// query parameter is accepted as a fallback.
func BearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	q := r.URL.Query()
	if t := q.Get("token"); t != "" {
		return t
	}
	return q.Get("access_token")
}

type ctxKey struct{}

// FromContext returns the authenticated principal.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// WriteError writes the standard {code,message} error body.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// Middleware requires a valid bearer token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := a.Lookup(BearerToken(r))
		if p == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			WriteError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// IsLoopback reports whether the request came from a loopback address.
func IsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Internal guards internal endpoints: loopback only and X-Internal-Token.
func (a *Authenticator) Internal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLoopback(r) {
			WriteError(w, http.StatusForbidden, "unauthorized", "internal endpoint is loopback only")
			return
		}
		tok := r.Header.Get("X-Internal-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		h := sha256.Sum256([]byte(tok))
		if subtle.ConstantTimeCompare(h[:], a.internalHash[:]) != 1 {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "bad internal token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
