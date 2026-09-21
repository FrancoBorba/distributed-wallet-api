/*
@Author: Franco Ribeiro Borba
@Description: Metrics of the service. Each one answers a question an operator
actually asks during an incident, which is the only reason to collect it.
Results by status say whether money is still moving. Duplicates say whether the
deduplication is doing its job, and a sudden drop to zero is as suspicious as a
spike. Conflicts say whether contention over a wallet is reaching the retry
limit. Dead letters say whether messages are being abandoned. Outbox lag is the
single most important one: it is the age of the oldest event that was committed
and not yet delivered, so it stays near zero while publication keeps up and
grows the instant it stops, which is exactly the failure that is invisible from
the outside because the API keeps answering normally. Reconciliation
divergences are the alarm of last resort: a wallet holding money its history
cannot explain.
@Date : 21/09/2026
@Update: -
*/
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Transport names the entry point an operation came through, so a problem can
// be attributed to HTTP or to the queue.
const (
	TransportHTTP      = "http"
	TransportSQS       = "sqs"
	TransportReference = "reference-worker"
)

// Metrics holds every collector of the service.
type Metrics struct {
	Operations   *prometheus.CounterVec
	Duplicates   *prometheus.CounterVec
	Conflicts    *prometheus.CounterVec
	Retries      *prometheus.CounterVec
	DeadLettered prometheus.Counter
	Latency      *prometheus.HistogramVec

	OutboxLag     prometheus.Gauge
	OutboxPending prometheus.Gauge

	Divergences prometheus.Counter
}

// New builds the collectors and registers them.
func New(registry prometheus.Registerer) *Metrics {
	m := &Metrics{
		Operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_operations_total",
			Help: "Wager operations by transport, kind and final state.",
		}, []string{"transport", "kind", "state"}),

		Duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_duplicates_total",
			Help: "Operations recognised as a repeat and not applied again.",
		}, []string{"transport", "reason"}),

		Conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_conflicts_total",
			Help: "Conflicts detected: concurrent writers of a wallet, or a key reused with another payload.",
		}, []string{"kind"}),

		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_retries_total",
			Help: "Attempts scheduled again after a failure, by component.",
		}, []string{"component"}),

		DeadLettered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_dead_lettered_total",
			Help: "Messages left for the dead letter queue because they can never succeed.",
		}),

		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wager_processing_duration_seconds",
			Help:    "Time to process one operation end to end, by transport.",
			Buckets: prometheus.DefBuckets,
		}, []string{"transport"}),

		OutboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wager_outbox_lag_seconds",
			Help: "Age of the oldest event that is committed and not yet published.",
		}),

		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wager_outbox_pending_events",
			Help: "Events committed and not yet published.",
		}),

		Divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_reconciliation_divergences_total",
			Help: "Reconciliations where the stored balance did not match the ledger.",
		}),
	}

	registry.MustRegister(
		m.Operations, m.Duplicates, m.Conflicts, m.Retries,
		m.DeadLettered, m.Latency, m.OutboxLag, m.OutboxPending, m.Divergences,
	)

	return m
}

// ObserveOperation records one concluded operation and how long it took.
func (m *Metrics) ObserveOperation(transport, kind, state string, elapsed time.Duration) {
	m.Operations.WithLabelValues(transport, kind, state).Inc()
	m.Latency.WithLabelValues(transport).Observe(elapsed.Seconds())
}

// ObserveDuplicate records an operation that was recognised as a repeat.
// Reason tells apart a replay caught by the business identity from a message
// caught by the inbox, which fail at different layers.
func (m *Metrics) ObserveDuplicate(transport, reason string) {
	m.Duplicates.WithLabelValues(transport, reason).Inc()
}

// ObserveConflict records a conflict.
func (m *Metrics) ObserveConflict(kind string) {
	m.Conflicts.WithLabelValues(kind).Inc()
}

// ObserveRetry records an attempt that was scheduled again.
func (m *Metrics) ObserveRetry(component string) {
	m.Retries.WithLabelValues(component).Inc()
}

// ObserveDeadLetter records a message left for the dead letter queue.
func (m *Metrics) ObserveDeadLetter() {
	m.DeadLettered.Inc()
}

// ObserveOutbox records the current state of the publication backlog.
func (m *Metrics) ObserveOutbox(lag time.Duration, pending int) {
	m.OutboxLag.Set(lag.Seconds())
	m.OutboxPending.Set(float64(pending))
}

// ObserveDivergence records a reconciliation that did not add up.
func (m *Metrics) ObserveDivergence() {
	m.Divergences.Inc()
}
