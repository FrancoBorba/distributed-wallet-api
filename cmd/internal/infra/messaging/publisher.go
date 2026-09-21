/*
@Author: Franco Ribeiro Borba
@Description: SQS publisher. It is the last step of the outbox: the event was
already committed, and this only carries it to the broker. Two FIFO settings
carry the guarantees. MessageGroupId is the aggregate the event belongs to, so
everything about one wallet is delivered in order while different wallets stay
fully parallel, which is the same shape as the row lock that serializes the
writes. MessageDeduplicationId is the eventId, so a republication after a crash
between the send and the confirmation is dropped by SQS itself inside its five
minute window; outside of it, the consumer still recognises the repeat by the
same eventId, which is why the durable identity matters more than the broker
feature. The event type also travels as a message attribute, so a consumer can
filter or route without parsing the body.
@Date : 20/09/2026
@Update: -
*/
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Publisher sends integration events to their destination queue.
type Publisher struct {
	client   *sqs.Client
	queueURL string
	fifo     bool
}

// NewPublisher builds the publisher for one queue. Whether the queue is FIFO is
// decided by its name, which is how SQS itself distinguishes them, so the same
// code serves a standard queue without carrying settings it would reject.
func NewPublisher(client *sqs.Client, queueURL string) *Publisher {
	return &Publisher{
		client:   client,
		queueURL: queueURL,
		fifo:     strings.HasSuffix(queueURL, ".fifo"),
	}
}

// Publish delivers one event. An error here is never fatal: the event stays in
// the outbox, its claim is released and another attempt takes it later, which
// is what makes a broker outage a delay instead of a loss.
func (p *Publisher) Publish(ctx context.Context, envelope events.Envelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", envelope.ID(), err)
	}

	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.queueURL),
		MessageBody: aws.String(string(body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType": {
				DataType:    aws.String("String"),
				StringValue: aws.String(string(envelope.Type())),
			},
			"correlationId": {
				DataType:    aws.String("String"),
				StringValue: aws.String(envelope.CorrelationID().String()),
			},
		},
	}

	if p.fifo {
		// Ordering is per aggregate. A consumer that needs the total order of
		// one wallet does not depend on it either way: WalletBalanceChanged
		// carries walletVersion, which orders the events of a wallet no matter
		// how they were grouped or redelivered.
		input.MessageGroupId = aws.String(envelope.AggregateID().String())
		input.MessageDeduplicationId = aws.String(envelope.ID().String())
	}

	if _, err := p.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("publish event %s: %w", envelope.ID(), err)
	}

	return nil
}
