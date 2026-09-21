/*
@Author: Franco Ribeiro Borba
@Description: Request middleware and the identity boundary.
@Date : 20/09/2026
@Update: -

IMPORTANT, READ BEFORE DEPLOYING ANYTHING:

Authentication is NOT implemented yet. TrustedHeaderAuthenticator believes
whatever the caller writes in a header, so any client can claim to be any
provider or the internal service. This is a development placeholder and the
service must not be exposed to an untrusted network in this state.

What IS implemented is the authorization side. The handlers already read the
identity from the context and enforce the isolation rules: a provider only ever
reaches its own transactions, and wallet operations are restricted to the
internal service. Those checks are real and tested, so the remaining work is to
replace where the identity comes from, not how it is used. The Authenticator
interface is that seam: a JWTAuthenticator validating a Keycloak token against
the JWKS of the realm, mapping the client to a providerId and a realm role to
the internal service, drops in with no change to any handler.
*/
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// contextKey is private, so nothing outside this package can put a forged
// identity into a request context.
type contextKey int

const (
	identityKey contextKey = iota
	correlationKey
)

// ErrUnauthenticated is a request that carries no usable credential.
var ErrUnauthenticated = errors.New("no valid credential was presented")

// Identity is who the caller is allowed to act as. ProviderID is the only
// provider whose data this caller may read or move; Internal marks the service
// itself, which is what opens wallets and reads any of them.
type Identity struct {
	ProviderID string
	Internal   bool
}

// Authenticator resolves the identity of a request. It is the single seam
// between the transport and the authorization rules.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// TrustedHeaderAuthenticator reads the identity from request headers.
//
// It performs no verification whatsoever and exists only so the authorization
// rules can be written and tested before the identity provider is wired. See
// the warning at the top of this file.
type TrustedHeaderAuthenticator struct{}

// Authenticate believes the headers. X-Service-Role: internal claims the
// internal service; X-Provider-Id claims a provider.
func (TrustedHeaderAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	if strings.EqualFold(r.Header.Get("X-Service-Role"), "internal") {
		return Identity{Internal: true}, nil
	}

	providerID := strings.TrimSpace(r.Header.Get("X-Provider-Id"))
	if providerID == "" {
		return Identity{}, ErrUnauthenticated
	}

	return Identity{ProviderID: providerID}, nil
}

// identityOf reads the identity a request was authenticated with.
func identityOf(ctx context.Context) Identity {
	identity, _ := ctx.Value(identityKey).(Identity)

	return identity
}

// correlationOf reads the trace identifier of a request.
func correlationOf(ctx context.Context) uuid.UUID {
	correlation, _ := ctx.Value(correlationKey).(uuid.UUID)

	return correlation
}

// mayActAs reports whether an identity may operate on behalf of a provider.
// The internal service may act for any of them; a provider only for itself.
func (i Identity) mayActAs(providerID string) bool {
	return i.Internal || (i.ProviderID != "" && i.ProviderID == providerID)
}

// authenticated wraps a handler so it only runs for a resolved identity.
func authenticated(auth Authenticator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, err := auth.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, CodeUnauthenticated, err.Error())

			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), identityKey, identity)))
	}
}

// internalOnly wraps a handler so only the service itself reaches it. Wallet
// operations live behind it: a provider moves money through the wagering
// endpoints and never touches a wallet directly.
func internalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !identityOf(r.Context()).Internal {
			writeError(w, http.StatusForbidden, CodeForbidden, "this operation is restricted to the internal service")

			return
		}

		next(w, r)
	}
}

// withCorrelation gives every request a trace identifier, reusing the one the
// caller sent when there is one. It is the same value that ends up in the
// events, the logs and the messages of the whole operation, which is what makes
// a distributed flow reconstructable afterwards.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlation, err := uuid.Parse(strings.TrimSpace(r.Header.Get("X-Correlation-Id")))
		if err != nil {
			correlation = uuid.New()
		}

		// Echoed back so the caller can quote it when reporting a problem.
		w.Header().Set("X-Correlation-Id", correlation.String())

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey, correlation)))
	})
}

// statusRecorder remembers the status so the log can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// withLogging records one JSON line per request, carrying the identifiers that
// tie it to everything else and never the body, which holds financial values.
func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		logger.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(started)),
			slog.String("correlationId", correlationOf(r.Context()).String()),
			slog.String("providerId", identityOf(r.Context()).ProviderID),
		)
	})
}

// withRecovery turns a panic into a 500 instead of a dropped connection, and
// keeps the process alive: one broken request must not take the consumer and
// the outbox publisher down with it.
func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("handler panicked",
					slog.Any("panic", recovered),
					slog.String("path", r.URL.Path),
					slog.String("correlationId", correlationOf(r.Context()).String()))

				writeError(w, http.StatusInternalServerError, CodeInternal, "the request could not be completed")
			}
		}()

		next.ServeHTTP(w, r)
	})
}
