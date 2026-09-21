/*
@Author: Franco Ribeiro Borba
@Description: HTTP router and server. The routes are declared with the method
patterns of net/http, so no router dependency is needed, and the middleware is
applied in the order a request needs it: recovery outermost so a panic anywhere
below still produces an answer, then the correlation identifier so every log
line and every event of the request carries it, then logging, which can only
report a status once the handlers below have produced one. Authentication is
applied per route rather than globally, because the probes have to answer
before any credential exists. Shutdown is graceful: the listener stops
accepting, requests already running are given the stop deadline to finish, and
only then does Fx close the pool they are using.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger/v2"
)

// NewRouter declares every route of the API.
func NewRouter(wallets *WalletHandler, wagering *WageringHandler, health *HealthHandler, auth Authenticator, registry *prometheus.Registry, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Public: an orchestrator calls these before any credential exists.
	mux.HandleFunc("GET /health/live", health.Live)
	mux.HandleFunc("GET /health/ready", health.Ready)

	// Metrics are public for the same reason the probes are: the collector
	// scrapes them from inside the cluster, before any business credential
	// exists. They carry counters and durations only, never a balance.
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Interactive documentation, which is also how the API is exercised by
	// hand during development.
	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, CodeNotFound, "unknown route")

			return
		}

		http.Redirect(w, r, "/swagger/index.html", http.StatusFound)
	})

	// Wallet operations are restricted to the internal service.
	mux.HandleFunc("POST /wallets", authenticated(auth, internalOnly(wallets.Open)))
	mux.HandleFunc("GET /wallets/{walletId}", authenticated(auth, internalOnly(wallets.Get)))
	mux.HandleFunc("GET /wallets/{walletId}/ledger", authenticated(auth, internalOnly(wallets.Ledger)))
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", authenticated(auth, internalOnly(wallets.Reconcile)))

	// Provider operations. The isolation between providers is enforced inside
	// each handler, because it depends on what the request is about.
	mux.HandleFunc("POST /wagering/transactions", authenticated(auth, wagering.Submit))
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", authenticated(auth, wagering.GetByID))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", authenticated(auth, wagering.GetByProvider))

	return withRecovery(logger, withCorrelation(withLogging(logger, mux)))
}

// Server is the HTTP server of the service.
type Server struct {
	server *http.Server
	logger *slog.Logger
}

// NewServer builds the server with the timeouts that keep a slow or idle
// client from holding a connection open indefinitely.
func NewServer(handler http.Handler, logger *slog.Logger, cfg config.HTTP) *Server {
	return &Server{
		server: &http.Server{
			Addr:              cfg.Address,
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		},
		logger: logger,
	}
}

// Start begins listening. The listener is opened synchronously, so a port
// already taken fails the startup of the application instead of being
// discovered later in a goroutine nobody is watching.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}

	s.logger.Info("http server listening",
		slog.String("address", listener.Addr().String()),
		slog.String("docs", "http://"+listener.Addr().String()+"/swagger/index.html"))

	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server stopped unexpectedly", slog.String("error", err.Error()))
		}
	}()

	return nil
}

// Stop closes the listener and waits for the requests in flight, up to the
// deadline of the given context.
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("stopping http server")

	return s.server.Shutdown(ctx)
}
