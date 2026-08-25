package middleware

import "net/http"

// cspPolicy is the strict Content-Security-Policy applied to every
// response. The shape is intentionally tight enough that adding a new
// external script/style/font source requires a code change here —
// surfacing supply-chain risk in code review rather than buried in
// HTML.
//
// 'unsafe-inline' on style-src is required by Tailwind's runtime CSS
// reset; we accept that exception with a comment because the script
// sources are still locked to 'self'.
const cspPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// CSP returns middleware that sets the Content-Security-Policy header
// plus companion hardening headers (`X-Content-Type-Options`,
// `Referrer-Policy`, `Cross-Origin-Resource-Policy`) on every response.
//
// These headers are emitted before the next handler runs so even error
// responses carry the same protection.
func CSP() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", cspPolicy)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			next.ServeHTTP(w, r)
		})
	}
}
