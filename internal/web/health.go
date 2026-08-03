package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

const (
	pathHealthz = "/healthz"
	pathReadyz  = "/readyz"
)

// readyTimeout bounds the database check. A probe that hangs is a probe that
// tells the orchestrator nothing, so an unanswered ping counts as not ready.
const readyTimeout = 2 * time.Second

// healthBody is the answer to both probes. `status` is the field a human reads;
// the HTTP status code is the field a machine reads.
type healthBody struct {
	Status string `json:"status"`
	// Detail is present only when something is wrong, and says what, in terms
	// that give nothing away about the internals.
	Detail string `json:"detail,omitempty"`
}

// handleHealthz answers as long as the process is running and able to serve.
// It deliberately touches nothing else: a liveness probe that fails when the
// database is down would have the orchestrator restart a perfectly healthy
// process, which cannot help and loses the in-flight requests.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, healthBody{Status: "ok"})
}

// handleReadyz reports whether this instance can actually do its job, which
// means reaching the database. A failure returns 503 so a load balancer takes
// the instance out of rotation without killing it.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		// The real error goes to the log, where it is useful. The response
		// says only that the database is unreachable: the probe is reachable
		// without authentication and connection strings and driver messages
		// are not something to hand out.
		Logger(r.Context()).Error("readiness check failed", slog.String("error", err.Error()))
		writeJSON(w, r, http.StatusServiceUnavailable, healthBody{
			Status: "unavailable",
			Detail: "database unreachable",
		})
		return
	}

	writeJSON(w, r, http.StatusOK, healthBody{Status: "ok"})
}
