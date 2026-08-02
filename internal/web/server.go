// Package web holds everything that speaks HTTP: routing, middleware and
// handlers.
//
// Handlers stay thin on purpose. Business logic lives below them, so that the
// phase 2 runner can drive the same logic from a background worker with no
// HTTP request in sight (DESIGN.md §10).
package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// Pinger is all this package needs from the database: a way to ask whether it
// is answering. Narrowing it here keeps HTTP code free of pgx and lets the
// health handlers be tested without a database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server carries the dependencies shared by all handlers.
type Server struct {
	logger *slog.Logger
	db     Pinger
}

// New returns a Server. Handler builds the routes.
func New(logger *slog.Logger, db Pinger) *Server {
	return &Server{logger: logger, db: db}
}

// Handler returns the fully wrapped HTTP handler for the application.
//
// Order matters: the id is assigned first so everything below can log it, the
// access log wraps the panic guard so that a recovered panic is still reported
// as the 500 it became, and the routes run innermost.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)

	return withRequestID(s.logger, logRequests(recoverPanic(mux)))
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+pathHealthz, s.handleHealthz)
	mux.HandleFunc("GET "+pathReadyz, s.handleReadyz)
}

// errorBody is the shape of every error this package returns. Mock traffic
// answers JSON, so the application answers JSON too rather than making a
// caller parse two different error formats.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON sends v with the given status. A body that fails to encode is
// logged rather than returned: the status line has already gone out, so there
// is no way left to report the failure to the client.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		Logger(r.Context()).Error("write response body", slog.String("error", err.Error()))
	}
}
