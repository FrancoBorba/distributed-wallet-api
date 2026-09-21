/*
@Author: Franco Ribeiro Borba
@Description: Composition of the service with Uber Fx. Every dependency is
built by a constructor and wired by type, which is what keeps the domain and
the use cases unaware that PostgreSQL, SQS or Fx exist at all: they receive
interfaces, and this file is the only place that decides which implementation
satisfies them. The lifecycle is the other half of its job. Workers start after
their dependencies are ready and are stopped before them, because Fx unwinds
the graph in reverse construction order, so the connection pool is closed only
once the consumer and the publisher have finished using it. Each worker owns a
context that is cancelled on stop and a channel that reports when its loop has
actually returned, so shutdown waits for the work in flight instead of assuming
it ended.
@Date : 20/09/2026
@Update: -
*/
package di

import (
	"context"
	"log/slog"
	nethttp "net/http"
	"os"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	httpapi "github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/http"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/messaging"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/postgres"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/worker"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
)

// Module is the whole service. The configuration is supplied by main, which
// loads it before anything is built so that a bad setting stops the process
// with a readable message instead of a dependency graph error.
var Module = fx.Options(
	httpModule,
	observabilityModule,
	persistenceModule,
	messagingModule,
	useCaseModule,
	workerModule,
)

// observabilityModule provides structured logging. Logs are JSON because they
// are read by machines first, and carry identifiers only: no credentials and no
// full financial payloads.
var observabilityModule = fx.Module("observability",
	fx.Provide(
		func() *slog.Logger {
			return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		},

		// A registry of our own rather than the global one, so the exposed
		// metrics are exactly the ones this service declares and a test can
		// build a second, independent set.
		prometheus.NewRegistry,
		func(registry *prometheus.Registry) *metrics.Metrics { return metrics.New(registry) },
	),
)

// persistenceModule provides the connection pool and everything built on it.
var persistenceModule = fx.Module("persistence",
	fx.Provide(
		providePool,
		postgres.NewUnitOfWork,
		postgres.NewOutboxStore,
		postgres.NewPendingReferenceStore,

		func(store *postgres.PendingReferenceStore) usecase.PendingReferenceStore { return store },

		// The use cases only ever see the interface, which is what lets a test
		// replace the whole transaction boundary with an in-memory one.
		func(uow *postgres.UnitOfWork) usecase.UnitOfWork { return uow },
	),
)

// messagingModule provides the broker client and both ends of the queue.
var messagingModule = fx.Module("messaging",
	fx.Provide(
		provideSQSClient,
		providePublisher,
		provideHandler,
		provideConsumer,
	),
)

// useCaseModule provides the application layer.
var useCaseModule = fx.Module("usecase",
	fx.Provide(
		func() usecase.Clock { return usecase.SystemClock{} },
		func() usecase.IDGenerator { return usecase.UUIDGenerator{} },
		usecase.NewProcessWagerTransaction,
		usecase.NewOpenWallet,
		usecase.NewResumePendingReference,
	),
)

// workerModule provides the background workers and starts them.
var workerModule = fx.Module("worker",
	fx.Provide(provideOutboxPublisher, provideReferenceResolver),
	fx.Invoke(startOutboxPublisher, startReferenceResolver, startConsumer),
)

// providePool opens the connection pool and closes it on shutdown. Fx stops
// components in reverse construction order, so this hook runs after the workers
// have stopped, never while one of them is still holding a connection.
func providePool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(context.Background(), cfg.Database)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			logger.Info("closing database pool")
			pool.Close()

			return nil
		},
	})

	return pool, nil
}

// provideSQSClient builds the client shared by the publisher and the consumer.
func provideSQSClient(cfg config.Config) (*sqs.Client, error) {
	return messaging.NewSQSClient(context.Background(), cfg.AWS)
}

// providePublisher binds the outbound queue to the Publisher port.
func providePublisher(client *sqs.Client, cfg config.Config) usecase.Publisher {
	return messaging.NewPublisher(client, cfg.AWS.EventsQueueURL)
}

// provideHandler binds the use case to the inbound queue.
func provideHandler(processor *usecase.ProcessWagerTransaction, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Config) messaging.MessageHandler {
	return messaging.NewWagerHandler(processor, logger, recorder, cfg.Consumer)
}

// provideConsumer builds the consumer of provider operations.
func provideConsumer(client *sqs.Client, handler messaging.MessageHandler, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Config) *messaging.Consumer {
	return messaging.NewConsumer(client, handler, logger, recorder, cfg.Consumer, cfg.AWS.InboundQueueURL)
}

// provideOutboxPublisher builds the worker that delivers committed events.
func provideOutboxPublisher(store *postgres.OutboxStore, publisher usecase.Publisher, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Config) *worker.OutboxPublisher {
	return worker.NewOutboxPublisher(store, publisher, logger, recorder, cfg.Outbox)
}

// startConsumer runs the SQS consumer for the lifetime of the application.
func startConsumer(lc fx.Lifecycle, consumer *messaging.Consumer, logger *slog.Logger) {
	runWorker(lc, logger, "sqs-consumer", consumer.Run)
}

// startOutboxPublisher runs the outbox worker for the lifetime of the
// application.
func startOutboxPublisher(lc fx.Lifecycle, publisher *worker.OutboxPublisher, logger *slog.Logger) {
	runWorker(lc, logger, "outbox-publisher", publisher.Run)
}

// runWorker registers a loop as a lifecycle component.
//
// The context it receives is cancelled on stop, which is how a worker learns to
// stop taking new work, and the done channel is how the stop hook learns that
// the loop has actually returned. Waiting on it is the difference between a
// shutdown that finished the work in flight and one that merely asked it to.
// When the stop deadline arrives first, the process gives up on waiting and
// whatever was in flight is recovered by another instance: an unconfirmed
// message becomes visible again and a claimed event has its lease expire.
func runWorker(lc fx.Lifecycle, logger *slog.Logger, name string, run func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				run(ctx)
			}()

			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			logger.Info("stopping worker", slog.String("worker", name))
			cancel()

			select {
			case <-done:
				logger.Info("worker stopped cleanly", slog.String("worker", name))

				return nil

			case <-stopCtx.Done():
				logger.Warn("worker did not stop within the deadline, its work will be redelivered",
					slog.String("worker", name))

				return nil
			}
		},
	})
}

// httpModule provides the API and starts it.
var httpModule = fx.Module("http",
	fx.Provide(
		postgres.NewQueryRepository,

		// The read side is one type behind two ports, so a use case only sees
		// the queries it actually needs.
		func(queries *postgres.QueryRepository) usecase.WalletQueries { return queries },
		func(queries *postgres.QueryRepository) usecase.TransactionQueries { return queries },

		usecase.NewReconcileWallet,
		provideAuthenticator,
		provideHealthChecks,
		httpapi.NewWalletHandler,
		httpapi.NewWageringHandler,
		httpapi.NewHealthHandler,
		httpapi.NewRouter,
		provideHTTPServer,
	),
	fx.Invoke(startHTTPServer),
)

// provideAuthenticator resolves the identity of a request.
//
// With OIDC enabled it verifies bearer tokens against the realm, which is the
// only mode meant to face anything but a developer laptop. Reading the realm
// metadata happens here, at startup, so an unreachable identity provider stops
// the boot rather than turning every request into a 401 later.
//
// The header mode is the development placeholder and believes whatever it is
// given. The warning is logged on purpose: an environment running without real
// authentication should say so loudly, not hide it in a configuration file.
func provideAuthenticator(logger *slog.Logger, cfg config.Config) (httpapi.Authenticator, error) {
	if !cfg.OIDC.Enabled {
		logger.Warn("authentication is NOT enabled: identities are read from request headers without verification",
			slog.String("set", "OIDC_ENABLED=true to verify real tokens"))

		return httpapi.TrustedHeaderAuthenticator{}, nil
	}

	authenticator, err := httpapi.NewJWTAuthenticator(context.Background(), cfg.OIDC)
	if err != nil {
		return nil, err
	}

	logger.Info("authentication enabled",
		slog.String("issuer", cfg.OIDC.Issuer),
		slog.String("audience", cfg.OIDC.Audience),
		slog.String("internalRole", cfg.OIDC.InternalRole))

	return authenticator, nil
}

// provideHealthChecks lists the dependencies the readiness probe verifies.
func provideHealthChecks(pool *pgxpool.Pool, client *sqs.Client, cfg config.Config) []httpapi.Check {
	return []httpapi.Check{
		{Name: "postgres", Probe: pool.Ping},
		{
			Name: "sqs",
			Probe: func(ctx context.Context) error {
				// Reading an attribute of the inbound queue proves both that
				// the broker answers and that this queue exists, which a plain
				// connection check would not.
				_, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
					QueueUrl:       &cfg.AWS.InboundQueueURL,
					AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
				})

				return err
			},
		},
	}
}

// provideHTTPServer builds the server over the router.
func provideHTTPServer(handler nethttp.Handler, logger *slog.Logger, cfg config.Config) *httpapi.Server {
	return httpapi.NewServer(handler, logger, cfg.HTTP)
}

// startHTTPServer binds the server to the application lifetime. Fx stops
// components in reverse construction order, so the server stops accepting and
// drains its requests before the connection pool those requests are using is
// closed.
func startHTTPServer(lc fx.Lifecycle, server *httpapi.Server) {
	lc.Append(fx.Hook{
		OnStart: server.Start,
		OnStop:  server.Stop,
	})
}

// provideReferenceResolver builds the worker that carries forward reversals
// waiting for the transaction they reverse.
func provideReferenceResolver(store usecase.PendingReferenceStore, resume *usecase.ResumePendingReference, logger *slog.Logger, recorder *metrics.Metrics, clock usecase.Clock, cfg config.Config) *worker.ReferenceResolver {
	return worker.NewReferenceResolver(store, resume, logger, recorder, clock, cfg.Reference)
}

// startReferenceResolver runs the reference worker for the lifetime of the
// application.
func startReferenceResolver(lc fx.Lifecycle, resolver *worker.ReferenceResolver, logger *slog.Logger) {
	runWorker(lc, logger, "reference-resolver", resolver.Run)
}
