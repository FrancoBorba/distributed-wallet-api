#!/bin/bash
#
# Queue provisioning, run by LocalStack once it is ready.
#
# Four queues, in two pairs. Each working queue has a dead letter queue behind
# it, and the redrive policy is what connects them: after maxReceiveCount
# deliveries that were never deleted, SQS moves the message aside instead of
# retrying it forever. That is where a malformed message and a message whose
# handling keeps failing both end up, and it is the reason a permanent failure
# is left in the queue rather than deleted: leaving it is what sends it here,
# where it can be inspected.
#
# All of them are FIFO. On the inbound side this gives ordering per
# MessageGroupId, which the producer sets to the wallet, so operations over one
# wallet are handled in the order they were sent while other wallets keep going
# in parallel. On the outbound side the service sets the group to the aggregate
# of the event and the deduplication id to the eventId, so a republication
# after a crash is dropped by the broker inside its five minute window.
#
# ContentBasedDeduplication stays off on purpose: deduplication must follow the
# identity of the operation, not a hash of the body the broker happens to
# compute, so both sides send an explicit MessageDeduplicationId.

set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
ACCOUNT="000000000000"

# maxReceiveCount is the retry budget of a message before the dead letter
# queue. Five is a compromise: enough for a dependency that blinks, few enough
# that a message which will never work stops circulating quickly.
MAX_RECEIVE_COUNT=5

# Longer than the handler timeout of the consumer, so a message is never handed
# to a second consumer while the first one is still working on it.
VISIBILITY_TIMEOUT=60

create_pair() {
  local name="$1"

  awslocal sqs create-queue \
    --queue-name "${name}-dlq.fifo" \
    --attributes FifoQueue=true,ContentBasedDeduplication=false,MessageRetentionPeriod=1209600

  local dlq_arn
  dlq_arn="arn:aws:sqs:${REGION}:${ACCOUNT}:${name}-dlq.fifo"

  awslocal sqs create-queue \
    --queue-name "${name}.fifo" \
    --attributes "$(cat <<JSON
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "VisibilityTimeout": "${VISIBILITY_TIMEOUT}",
  "MessageRetentionPeriod": "1209600",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVE_COUNT}\"}"
}
JSON
)"

  echo "queue ready: ${name}.fifo (dlq: ${name}-dlq.fifo, maxReceiveCount=${MAX_RECEIVE_COUNT})"
}

# Inbound: provider operations consumed by the service.
create_pair "wager-transactions"

# Outbound: the integration events the outbox publishes.
create_pair "wallet-events"

awslocal sqs list-queues
