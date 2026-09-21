/*
@Author: Franco Ribeiro Borba
@Description: Configuration read from the environment. Everything the service
needs to reach PostgreSQL and SQS, plus the timings that decide how the workers
behave, is resolved and validated once at startup, so a missing queue URL or an
impossible timeout stops the process immediately instead of surfacing as a
failure in the middle of a financial operation. Two settings are tied together
on purpose and checked here: the visibility timeout of the consumer must be
longer than the time a handler is allowed to take, otherwise SQS hands the same
message to a second consumer while the first one is still working on it.
Defaults are the ones that make the Docker Compose environment run with no
environment file at all.
@Date : 20/09/2026
@Update: -
*/
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrMissingSetting is a required environment variable that was not provided.
var ErrMissingSetting = errors.New("missing required setting")

// Config is the whole configuration of the service.
type Config struct {
	Database  Database
	AWS       AWS
	Consumer  Consumer
	Outbox    Outbox
	HTTP      HTTP
	Reference Reference
	OIDC      OIDC
	// ShutdownTimeout is how long the process waits for workers to finish
	// their current work before giving up on a graceful stop.
	ShutdownTimeout time.Duration
}

// HTTP is how the API is served. The timeouts keep a slow or idle client from
// holding a connection open indefinitely, which is what a connection pool of a
// fixed size cannot afford.
type HTTP struct {
	Address           string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Database is how to reach PostgreSQL.
type Database struct {
	URL string
	// MaxConnections bounds the pool. Every worker and every request shares
	// it, so it has to be large enough for the consumer concurrency plus the
	// publisher batch.
	MaxConnections int32
	ConnectTimeout time.Duration
}

// AWS is how to reach SQS. Endpoint is what points the SDK at LocalStack
// instead of the real service, and is empty in a real deployment.
type AWS struct {
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	// InboundQueueURL is the FIFO queue of provider operations.
	InboundQueueURL string
	// EventsQueueURL is the destination of the integration events this
	// service publishes.
	EventsQueueURL string
}

// Consumer is the behaviour of the SQS consumer.
type Consumer struct {
	// Name is the identity of this consumer in the inbox. Every instance of
	// the service shares it, because they are one logical consumer competing
	// for the same messages.
	Name string
	// MaxMessages is how many messages one receive call asks for.
	MaxMessages int32
	// WaitTime is the long polling window. A long wait means fewer empty
	// receives and a much lower cost than polling in a tight loop.
	WaitTime time.Duration
	// VisibilityTimeout is how long a received message stays invisible to
	// other consumers. It must outlast HandlerTimeout.
	VisibilityTimeout time.Duration
	// HandlerTimeout bounds the handling of a single message.
	HandlerTimeout time.Duration
	// Concurrency is how many messages are handled at the same time.
	Concurrency int
}

// Outbox is the behaviour of the publisher worker.
type Outbox struct {
	// PublisherID identifies this instance in the claim, so an abandoned lease
	// can be traced back to the process that took it.
	PublisherID string
	// BatchSize is how many events one claim takes.
	BatchSize int
	// PollInterval is how long the worker sleeps when it finds nothing to do.
	PollInterval time.Duration
	// ClaimLease is how long a claim holds an event. A publisher that dies
	// blocks its events for at most this long.
	ClaimLease time.Duration
	// RetryBaseDelay is the first backoff delay after a failed publication.
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps the exponential backoff.
	RetryMaxDelay time.Duration
}

// Reference is the behaviour of the worker that resolves reversals waiting for
// the transaction they reverse.
type Reference struct {
	// WorkerID identifies this instance in the claim.
	WorkerID string
	// BatchSize is how many waiting reversals one claim takes.
	BatchSize int
	// PollInterval is how long the worker sleeps when it finds nothing.
	PollInterval time.Duration
	// ClaimLease is how long a claim holds a reversal.
	ClaimLease time.Duration
	// RetryBaseDelay is the first backoff delay between attempts.
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps the exponential backoff.
	RetryMaxDelay time.Duration
	// MaxAttempts bounds how many times a reference is looked for. Zero
	// disables the limit and leaves only the deadline.
	MaxAttempts int
	// TTL is how long a reversal may wait in total, counted from its
	// creation. It bounds the wait even for a worker that restarted so often
	// that its attempt count never grew.
	TTL time.Duration
}

// OIDC is how tokens are verified. Issuer points at the realm of the identity
// provider, and the service reads its signing keys from there once and caches
// them, so verifying a token costs no network call.
type OIDC struct {
	// Enabled turns real authentication on. When it is false the service
	// trusts request headers, which is a development mode only.
	Enabled bool
	// Issuer is the realm URL, for example
	// http://localhost:8080/realms/wager.
	Issuer string
	// DiscoveryURL is where the realm metadata is read from, when that is not
	// the issuer itself. They differ whenever the service reaches the identity
	// provider through a name the tokens do not carry, which is the normal case
	// inside Docker: the container resolves it as "keycloak" while the tokens
	// say "localhost". The issuer of every token is still checked against
	// Issuer, so this only changes where the public keys are fetched from.
	DiscoveryURL string
	// Audience is the value every accepted token must carry in aud, which is
	// what stops a token minted for another service being replayed here.
	Audience string
	// InternalRole is the realm role that marks the internal service.
	InternalRole string
	// ProviderClaim is the claim that carries the provider identity. It
	// defaults to azp, the authorized party, which Keycloak sets to the
	// client that asked for the token.
	ProviderClaim string
	// DiscoveryTimeout bounds the startup call that reads the realm metadata.
	DiscoveryTimeout time.Duration
}

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		Database: Database{
			URL:            env("DATABASE_URL", "postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable"),
			MaxConnections: int32(envInt("DB_MAX_CONNECTIONS", 20)),
			ConnectTimeout: envDuration("DB_CONNECT_TIMEOUT", 10*time.Second),
		},
		AWS: AWS{
			Region:          env("AWS_REGION", "us-east-1"),
			Endpoint:        env("AWS_ENDPOINT_URL", "http://localhost:4566"),
			AccessKeyID:     env("AWS_ACCESS_KEY_ID", "test"),
			SecretAccessKey: env("AWS_SECRET_ACCESS_KEY", "test"),
			InboundQueueURL: env("SQS_INBOUND_QUEUE_URL", "http://localhost:4566/000000000000/wager-transactions.fifo"),
			EventsQueueURL:  env("SQS_EVENTS_QUEUE_URL", "http://localhost:4566/000000000000/wallet-events.fifo"),
		},
		Consumer: Consumer{
			Name:              env("CONSUMER_NAME", "wager-transactions-consumer"),
			MaxMessages:       int32(envInt("CONSUMER_MAX_MESSAGES", 10)),
			WaitTime:          envDuration("CONSUMER_WAIT_TIME", 20*time.Second),
			VisibilityTimeout: envDuration("CONSUMER_VISIBILITY_TIMEOUT", 60*time.Second),
			HandlerTimeout:    envDuration("CONSUMER_HANDLER_TIMEOUT", 30*time.Second),
			Concurrency:       envInt("CONSUMER_CONCURRENCY", 4),
		},
		Outbox: Outbox{
			PublisherID:    env("OUTBOX_PUBLISHER_ID", defaultPublisherID()),
			BatchSize:      envInt("OUTBOX_BATCH_SIZE", 50),
			PollInterval:   envDuration("OUTBOX_POLL_INTERVAL", time.Second),
			ClaimLease:     envDuration("OUTBOX_CLAIM_LEASE", 30*time.Second),
			RetryBaseDelay: envDuration("OUTBOX_RETRY_BASE_DELAY", 2*time.Second),
			RetryMaxDelay:  envDuration("OUTBOX_RETRY_MAX_DELAY", 5*time.Minute),
		},
		HTTP: HTTP{
			Address:           env("HTTP_ADDRESS", ":8081"),
			ReadHeaderTimeout: envDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		},
		Reference: Reference{
			WorkerID:       env("REFERENCE_WORKER_ID", defaultWorkerID("reference-resolver")),
			BatchSize:      envInt("REFERENCE_BATCH_SIZE", 25),
			PollInterval:   envDuration("REFERENCE_POLL_INTERVAL", 2*time.Second),
			ClaimLease:     envDuration("REFERENCE_CLAIM_LEASE", 30*time.Second),
			RetryBaseDelay: envDuration("REFERENCE_RETRY_BASE_DELAY", 2*time.Second),
			RetryMaxDelay:  envDuration("REFERENCE_RETRY_MAX_DELAY", 2*time.Minute),
			MaxAttempts:    envInt("REFERENCE_MAX_ATTEMPTS", 10),
			TTL:            envDuration("REFERENCE_TTL", 30*time.Minute),
		},
		OIDC: OIDC{
			Enabled:          envBool("OIDC_ENABLED", true),
			Issuer:           env("OIDC_ISSUER", "http://localhost:8080/realms/wager"),
			DiscoveryURL:     env("OIDC_DISCOVERY_URL", ""),
			Audience:         env("OIDC_AUDIENCE", "wallet-api"),
			InternalRole:     env("OIDC_INTERNAL_ROLE", "wallet-internal"),
			ProviderClaim:    env("OIDC_PROVIDER_CLAIM", "azp"),
			DiscoveryTimeout: envDuration("OIDC_DISCOVERY_TIMEOUT", 30*time.Second),
		},
		ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 30*time.Second),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validate refuses a configuration that cannot work, before anything connects.
func (c Config) validate() error {
	switch {
	case strings.TrimSpace(c.Database.URL) == "":
		return fmt.Errorf("%w: DATABASE_URL", ErrMissingSetting)
	case strings.TrimSpace(c.AWS.InboundQueueURL) == "":
		return fmt.Errorf("%w: SQS_INBOUND_QUEUE_URL", ErrMissingSetting)
	case strings.TrimSpace(c.AWS.EventsQueueURL) == "":
		return fmt.Errorf("%w: SQS_EVENTS_QUEUE_URL", ErrMissingSetting)
	case strings.TrimSpace(c.Consumer.Name) == "":
		return fmt.Errorf("%w: CONSUMER_NAME", ErrMissingSetting)
	case c.Database.MaxConnections < 1:
		return errors.New("DB_MAX_CONNECTIONS must be at least 1")
	case c.Consumer.Concurrency < 1:
		return errors.New("CONSUMER_CONCURRENCY must be at least 1")
	case c.Consumer.MaxMessages < 1 || c.Consumer.MaxMessages > 10:
		return errors.New("CONSUMER_MAX_MESSAGES must be between 1 and 10")
	case c.Consumer.WaitTime > 20*time.Second:
		return errors.New("CONSUMER_WAIT_TIME cannot exceed the 20s long polling limit of SQS")
	case strings.TrimSpace(c.HTTP.Address) == "":
		return fmt.Errorf("%w: HTTP_ADDRESS", ErrMissingSetting)
	case c.Reference.BatchSize < 1:
		return errors.New("REFERENCE_BATCH_SIZE must be at least 1")
	case c.Reference.MaxAttempts < 0:
		return errors.New("REFERENCE_MAX_ATTEMPTS cannot be negative")
	case c.Reference.MaxAttempts == 0 && c.Reference.TTL <= 0:
		return errors.New("a pending reference needs a bound: set REFERENCE_MAX_ATTEMPTS or REFERENCE_TTL")
	case c.OIDC.Enabled && strings.TrimSpace(c.OIDC.Issuer) == "":
		return fmt.Errorf("%w: OIDC_ISSUER", ErrMissingSetting)
	case c.OIDC.Enabled && strings.TrimSpace(c.OIDC.Audience) == "":
		return fmt.Errorf("%w: OIDC_AUDIENCE", ErrMissingSetting)
	case c.Outbox.BatchSize < 1:
		return errors.New("OUTBOX_BATCH_SIZE must be at least 1")
	}

	// A message becomes visible again while its handler is still running if
	// the timeout is not the longer of the two, and a second consumer then
	// works on an operation that is already in flight.
	if c.Consumer.VisibilityTimeout <= c.Consumer.HandlerTimeout {
		return fmt.Errorf(
			"CONSUMER_VISIBILITY_TIMEOUT (%s) must be longer than CONSUMER_HANDLER_TIMEOUT (%s)",
			c.Consumer.VisibilityTimeout, c.Consumer.HandlerTimeout,
		)
	}

	// The same reasoning on the publishing side: a lease that expires while
	// the event is being sent lets a second publisher send it again.
	if c.Outbox.ClaimLease <= c.Outbox.PollInterval {
		return fmt.Errorf(
			"OUTBOX_CLAIM_LEASE (%s) must be longer than OUTBOX_POLL_INTERVAL (%s)",
			c.Outbox.ClaimLease, c.Outbox.PollInterval,
		)
	}

	return nil
}

// defaultWorkerID names an instance after the host, which is the container id
// under Docker and is enough to tell competing workers apart in a claim.
func defaultWorkerID(role string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return role
	}

	return role + "@" + host
}

// envBool reads a boolean variable. Anything unreadable keeps the default,
// because a typo must not silently turn a security control off.
func envBool(key string, fallback bool) bool {
	value, err := strconv.ParseBool(env(key, ""))
	if err != nil {
		return fallback
	}

	return value
}

// defaultPublisherID names the instance after the host, which is the container
// id under Docker and is enough to tell competing publishers apart.
func defaultPublisherID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "outbox-publisher"
	}

	return "outbox-publisher@" + host
}

// env reads a variable, falling back to a default.
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	return fallback
}

// envInt reads an integer variable, falling back on anything unreadable.
func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(env(key, ""))
	if err != nil {
		return fallback
	}

	return value
}

// envDuration reads a duration such as "20s" or "5m".
func envDuration(key string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(env(key, ""))
	if err != nil {
		return fallback
	}

	return value
}
