package auth

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
)

// Cookie names for the session+CSRF pair. Underscore prefix is the
// convention used by the spec and mirrored in the portal's JS code
// (the CSRF reader looks for `__aplexica_csrf` by name).
const (
	CookieSession = "__Host-aplexica_session"
	CookieCSRF    = "__Host-aplexica_csrf"
)

// Handler bundles the /api/auth/{bootstrap,session,logout} endpoints.
// Mounted onto the server's root multiplexer because these routes
// MUST be reachable without an existing session cookie — they are the
// only paths that mint cookies in the first place.
type Handler struct {
	Tokens   *TokenStore
	Sessions *SessionStore
	Version  string
}

// bootstrapReq is the wire shape for POST /api/auth/bootstrap.
type bootstrapReq struct {
	Token string `json:"token"`
}

// whoami is the wire shape returned by both /bootstrap and /session.
type whoami struct {
	User   string         `json:"user"`
	Daemon map[string]any `json:"daemon"`
	Mode   string         `json:"mode"`
}

// Register attaches the auth handlers to mux. Implements
// web.HandlerRegistrar so the package wiring is one line in server.go.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/bootstrap", h.bootstrap)
	mux.HandleFunc("POST /api/auth/bootstrap-form", h.bootstrapForm)
	mux.HandleFunc("GET /api/auth/session", h.whoami)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
}

func (h *Handler) bootstrapForm(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", 413)
		} else {
			http.Error(w, "bad request", 400)
		}
		return
	}
	if len(r.Form) != 1 || len(r.Form["token"]) != 1 || r.Form.Get("token") == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if err := h.Tokens.Consume(r.Form.Get("token")); err != nil {
		if errors.Is(err, ErrTokenBusy) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", 429)
		} else {
			http.Error(w, "unauthorized", 401)
		}
		return
	}
	sid, csrf, err := h.Sessions.Create("local")
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	setSessionCookies(w, sid, csrf, h.Sessions.TTL())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	_, _ = io.WriteString(w, `<!doctype html><meta http-equiv="refresh" content="1;url=/"><title>Aplexica</title><p>Authenticated. <a href="/">Continue to Aplexica</a></p>`)
}

// bootstrap consumes the one-time URL token from the request body,
// mints a new session, and sets the session + CSRF cookies. On a bad
// or replayed token the response is 401 with no Set-Cookie.
func (h *Handler) bootstrap(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req bootstrapReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF || req.Token == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := h.Tokens.Consume(req.Token); err != nil {
		if errors.Is(err, ErrTokenBusy) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		// Distinguish expired (probably-benign timeout) from unknown
		// (potentially-malicious replay) only in logs; 401 is the
		// uniform external response.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid, csrf, err := h.Sessions.Create("local")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setSessionCookies(w, sid, csrf, h.Sessions.TTL())
	writeJSON(w, http.StatusOK, whoami{
		User:   "local",
		Daemon: map[string]any{"version": h.Version},
		Mode:   "local",
	})
}

// whoami returns the current session's identity, or 401 if no valid
// session cookie is present. This endpoint is the SPA's reconnect
// probe — after page reload, fetch /api/auth/session to confirm the
// stored session is still valid before rendering authenticated UI.
func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFromCookie(r, h.Sessions)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, whoami{
		User:   sess.User,
		Daemon: map[string]any{"version": h.Version},
		Mode:   "local",
	})
}

// logout deletes the server-side session entry and instructs the
// browser to drop both cookies. Always returns 204 — clients should
// treat the call as fire-and-forget.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieSession); err == nil {
		h.Sessions.Revoke(c.Value)
	}
	clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// setSessionCookies emits the session + CSRF Set-Cookie headers on
// w. The session cookie is HttpOnly + SameSite=Strict; the CSRF
// cookie is JS-readable (SameSite=Strict + non-HttpOnly) for the
// double-submit token pattern.
func setSessionCookies(w http.ResponseWriter, sid, csrf string, ttl time.Duration) {
	maxAge := int(ttl.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     CookieSession,
		Value:    sid,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Path:     "/",
		MaxAge:   maxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CookieCSRF,
		Value:    csrf,
		HttpOnly: false,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Path:     "/",
		MaxAge:   maxAge,
	})
}

// clearSessionCookies emits Set-Cookie headers that expire both
// cookies immediately. The browser drops them on the next request.
func clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{CookieSession, CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name:   name,
			Value:  "",
			Path:   "/",
			Secure: true,
			MaxAge: -1,
		})
	}
}

// sessionFromCookie extracts the session from the request's session
// cookie, returning false when the cookie is missing or the session
// is unknown/expired. Used by the whoami handler AND by the
// middleware.RequireSession enforcement.
func sessionFromCookie(r *http.Request, sessions *SessionStore) (Session, bool) {
	c, err := r.Cookie(CookieSession)
	if err != nil {
		return Session{}, false
	}
	return sessions.Get(c.Value)
}

// SessionFromCookie is the exported counterpart to sessionFromCookie
// for the middleware package, which needs the same lookup logic but
// lives in a different package.
func SessionFromCookie(r *http.Request, sessions *SessionStore) (Session, bool) {
	return sessionFromCookie(r, sessions)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ErrCSRFMismatch is returned by middleware when the double-submit
// CSRF token in the header does not match the cookie. Exposed for
// cross-package error comparison; the wire response is 403.
var ErrCSRFMismatch = errors.New("auth: csrf mismatch")
