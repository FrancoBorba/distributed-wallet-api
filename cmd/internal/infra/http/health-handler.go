/*
@Author: Franco Ribeiro Borba
@Description: Health probes. The two answer different questions and must not
be confused: liveness asks whether the process is running, and a failure means
restart me; readiness asks whether the dependencies answer, and a failure means
stop sending me traffic but leave me alone. Restarting a process because
PostgreSQL is briefly unreachable would turn a database blip into an outage of
every instance at once, which is why liveness never touches a dependency. The
probes are public, because an orchestrator calls them before any credential
exists, and they report only whether each dependency answered, never an address
or a driver message.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readinessTimeout bounds the probes. A dependency that has not answered by
// then is not ready, whatever it is doing.
const readinessTimeout = 3 * time.Second

// Check is one dependency and how to ask whether it answers.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// HealthHandler serves the probes.
type HealthHandler struct {
	checks []Check
	logger *slog.Logger
}

// NewHealthHandler builds the handler over the dependencies to verify.
func NewHealthHandler(checks []Check, logger *slog.Logger) *HealthHandler {
	return &HealthHandler{checks: checks, logger: logger}
}

// Live godoc
//
//	@Summary		Liveness probe
//	@Description	Reports that the process is running. It touches no dependency, because a restart would not fix a database that is briefly unreachable.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	HealthResponse
//	@Router			/health/live [get]
func (h *HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// Ready godoc
//
//	@Summary		Readiness probe
//	@Description	Reports whether PostgreSQL and SQS answer. A failure returns 503 so the instance stops receiving traffic without being restarted.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	HealthResponse
//	@Failure		503	{object}	HealthResponse
//	@Router			/health/ready [get]
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	response := HealthResponse{Status: "ok", Checks: make(map[string]string, len(h.checks))}
	status := http.StatusOK

	for _, check := range h.checks {
		if err := check.Probe(ctx); err != nil {
			h.logger.Warn("readiness check failed",
				slog.String("dependency", check.Name),
				slog.String("error", err.Error()))

			// The caller learns which dependency is down, never why: the
			// message could carry a host or a credential.
			response.Checks[check.Name] = "unavailable"
			response.Status = "degraded"
			status = http.StatusServiceUnavailable

			continue
		}

		response.Checks[check.Name] = "ok"
	}

	writeJSON(w, status, response)
}
