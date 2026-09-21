/*
@Author: Franco Ribeiro Borba
@Description: SQS client. The only difference between the local environment and
a real deployment lives here: LocalStack is reached by overriding the endpoint
of the SDK, while everything above this file talks to the same API and behaves
the same way. Credentials are static because LocalStack accepts any pair and a
real deployment resolves them from the environment, the instance role or the
container role through the default chain, which this configuration keeps
available. Queue access itself is a matter of broker credentials and policies,
not of application code: the consumer keeps validating every operation in the
domain regardless of who managed to put a message on the queue.
@Date : 20/09/2026
@Update: -
*/
package messaging

import (
	"context"
	"fmt"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// NewSQSClient builds the client every publisher and consumer shares.
func NewSQSClient(ctx context.Context, cfg config.AWS) (*sqs.Client, error) {
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}

	// Explicit credentials are what LocalStack needs; leaving them empty in a
	// real deployment lets the default chain resolve the role of the instance.
	if cfg.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}

	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
	}), nil
}
