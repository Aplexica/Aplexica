// Package api implements the /api/* REST handlers for the local web
// UI. Each endpoint group (daemon, agents, events, rules,
// conflicts, pending, config, transport, onboarding) lives in its own
// file and exposes a struct + constructor + Register method satisfying
// web.HandlerRegistrar.
//
// All handlers run BEHIND the protected mux mounted at /api/ by
// web.Server.UseProtected, so RequireSession + RequireCSRF have already
// fired before they execute. Handlers do not need to re-check auth.
//
// Wire shape for errors is uniform: { "error": "<msg>", "code":
// "<short>" } at the appropriate HTTP status. See WriteError below.
package api

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the JSON envelope returned for any non-2xx response.
// Code is a short machine-readable identifier ("validation",
// "not_found", "not_yet_implemented", "internal") so the SPA can
// branch on it without parsing the human-readable Error message.
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// WriteJSON serialises body as JSON with status. Sets Content-Type;
// silently ignores encoder errors because the response is already in
// flight by the time a marshal failure could surface.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError writes the standard error envelope at status. msg is the
// human-readable text; code is the short machine identifier.
func WriteError(w http.ResponseWriter, status int, msg, code string) {
	WriteJSON(w, status, ErrorBody{Error: msg, Code: code})
}
