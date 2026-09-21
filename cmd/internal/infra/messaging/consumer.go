/*
@Author: Franco Ribeiro Borba
@Description: SQS consumer. It long polls the queue, hands each message to the
handler and only deletes it after the handling was durably committed, which is
what makes a crash between the work and the delete a redelivery instead of a
lost operation. Failures are told apart on purpose: a transient one leaves the
message alone so it becomes visible again and is retried with a backoff based
on how many times it was already received, while a permanent one is left to the
redrive policy of the queue, which moves it to the dead letter queue once the
receive count is exhausted. A business rejection is not a failure at all, it is
a decision that was committed, so the message is deleted. On shutdown the
consumer stops fetching immediately but lets the work already in flight finish
on a context of its own, so a SIGTERM never cuts a financial operation in half.
@Date : 20/09/2026
@Update: -
*/
package messaging

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ErrPermanentMessage marks a message that can never succeed, such as one that
// breaks the contract or reuses an identifier with a different body. Retrying
// it would only delay its arrival at the dead letter queue.
var ErrPermanentMessage = errors.New("permanent message failure")

// receiveBackoff is how long the consumer waits after a failed receive, so a
// broker outage does not turn into a tight loop of failing calls.
const receiveBackoff = 2 * time.Second

// MessageHandler handles the body of one message. It returns nil when the
// handling was committed, a permanent error when the message is hopeless and
// any other error when another attempt may succeed.
type MessageHandler interface {
	Handle(ctx context.Context, body []byte) error
}

// Consumer reads provider operations from a queue.
type Consumer struct {
	client   *sqs.Client
	handler  MessageHandler
	logger   *slog.Logger
	metrics  *metrics.Metrics
	cfg      config.Consumer
	queueURL string
}

// NewConsumer builds the consumer of the inbound queue.
func NewConsumer(client *sqs.Client, handler MessageHandler, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Consumer, queueURL string) *Consumer {
	return &Consumer{
		client:   client,
		handler:  handler,
		logger:   logger,
		metrics:  recorder,
		cfg:      cfg,
		queueURL: queueURL,
	}
}

// Run polls the queue until the context is cancelled, then returns once the
// messages already being handled are done.
func (c *Consumer) Run(ctx context.Context) {
	var inFlight sync.WaitGroup

	// The batch is bounded by the configured concurrency, so the pool of
	// database connections is never asked for more than it can give.
	slots := make(chan struct{}, c.cfg.Concurrency)

	defer inFlight.Wait()

	for {
		if ctx.Err() != nil {
			c.logger.Info("consumer stopped fetching, waiting for messages in flight")
			return
		}

		messages, err := c.receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			c.logger.Error("receive from queue failed", slog.String("error", err.Error()))
			sleep(ctx, receiveBackoff)

			continue
		}

		for _, message := range messages {
			// Shutdown started while the batch was being dispatched. What was
			// not started is given back to the queue at once, instead of
			// waiting out the visibility timeout.
			if ctx.Err() != nil {
				c.release(message)
				continue
			}

			slots <- struct{}{}
			inFlight.Add(1)

			go func(message types.Message) {
				defer inFlight.Done()
				defer func() { <-slots }()

				c.handle(ctx, message)
			}(message)
		}
	}
}

// receive asks for a batch with long polling, which keeps the queue cheap: one
// call waits for work instead of many calls finding none.
func (c *Consumer) receive(ctx context.Context) ([]types.Message, error) {
	output, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.queueURL),
		MaxNumberOfMessages: c.cfg.MaxMessages,
		WaitTimeSeconds:     int32(c.cfg.WaitTime.Seconds()),
		VisibilityTimeout:   int32(c.cfg.VisibilityTimeout.Seconds()),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
		},
	})
	if err != nil {
		return nil, err
	}

	return output.Messages, nil
}

// handle runs one message to a conclusion.
func (c *Consumer) handle(ctx context.Context, message types.Message) {
	// The handling gets a context of its own, detached from the shutdown
	// signal and bounded by its own timeout, so that stopping the service
	// interrupts fetching and not a transaction that is already running. The
	// timeout is shorter than the visibility timeout, which is what keeps a
	// second consumer from picking up a message still being worked on.
	handleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.HandlerTimeout)
	defer cancel()

	logger := c.logger.With(slog.String("sqsMessageId", aws.ToString(message.MessageId)))

	err := c.handler.Handle(handleCtx, []byte(aws.ToString(message.Body)))

	switch {
	case err == nil:
		// The work is committed. Removing the message now is safe, and a crash
		// before this point simply means a redelivery the inbox recognises.
		c.delete(handleCtx, message, logger)

	case errors.Is(err, ErrPermanentMessage):
		// Nothing will make this message work. It is left in the queue so the
		// redrive policy moves it to the dead letter queue, where it can be
		// inspected instead of disappearing.
		logger.Error("permanent failure, leaving message for the dead letter queue",
			slog.String("error", err.Error()))

		c.metrics.ObserveDeadLetter()

	default:
		// Transient: the database or the broker was briefly unavailable. The
		// message becomes visible again, later each time it comes back.
		delay := c.retryDelay(message)

		c.metrics.ObserveRetry("sqs-consumer")
		logger.Warn("transient failure, message will be retried",
			slog.String("error", err.Error()),
			slog.Duration("retryIn", delay))

		c.changeVisibility(handleCtx, message, delay)
	}
}

// delete removes a handled message from the queue.
func (c *Consumer) delete(ctx context.Context, message types.Message, logger *slog.Logger) {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: message.ReceiptHandle,
	})
	if err != nil {
		// The work is already committed, so this only means the message will
		// be delivered again and recognised as a duplicate by the inbox.
		logger.Warn("delete message failed, it will be redelivered and deduplicated",
			slog.String("error", err.Error()))
	}
}

// release hands a message back immediately, used when shutdown starts before
// the handling does.
func (c *Consumer) release(message types.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c.changeVisibility(ctx, message, 0)
}

// changeVisibility sets when a message becomes visible again.
func (c *Consumer) changeVisibility(ctx context.Context, message types.Message, delay time.Duration) {
	_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.queueURL),
		ReceiptHandle:     message.ReceiptHandle,
		VisibilityTimeout: int32(delay.Seconds()),
	})
	if err != nil {
		// Not fatal: the message still becomes visible when its original
		// timeout expires, only later than we asked for.
		c.logger.Debug("change message visibility failed", slog.String("error", err.Error()))
	}
}

// retryDelay grows the wait with the number of deliveries already attempted,
// which spreads the retries of a broken dependency instead of hammering it.
func (c *Consumer) retryDelay(message types.Message) time.Duration {
	received := 1
	if value, ok := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; ok {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			received = parsed
		}
	}

	delay := time.Duration(1<<min(received-1, 6)) * time.Second

	// Never past the visibility timeout the queue was configured with, which
	// is the longest a message may stay hidden.
	if delay > c.cfg.VisibilityTimeout {
		return c.cfg.VisibilityTimeout
	}

	return delay
}

// sleep waits unless the context ends first.
func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
