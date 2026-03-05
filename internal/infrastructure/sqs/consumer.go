package sqsconsumer

import (
	"context"
	"encoding/json"

	"quiccpos/agent/internal/application/order"
	orderdomain "quiccpos/agent/internal/domain/order"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/rs/zerolog"
)

type Consumer struct {
	client       *sqs.Client
	queueURL     string
	orderService *order.Service
	logger       zerolog.Logger
}

func NewConsumer(client *sqs.Client, queueURL string, orderService *order.Service, logger zerolog.Logger) *Consumer {
	return &Consumer{
		client:       client,
		queueURL:     queueURL,
		orderService: orderService,
		logger:       logger.With().Str("component", "sqs-consumer").Logger(),
	}
}

// Start begins the long-poll loop. Blocks until ctx is cancelled.
func (c *Consumer) Start(ctx context.Context) {
	c.logger.Info().Str("queue", c.queueURL).Msg("Starting SQS consumer")

	for {
		select {
		case <-ctx.Done():
			c.logger.Info().Msg("SQS consumer stopped")
			return
		default:
		}

		output, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Error().Err(err).Msg("Error receiving SQS messages")
			continue
		}

		for _, msg := range output.Messages {
			c.processMessage(ctx, msg)
		}
	}
}

func (c *Consumer) processMessage(ctx context.Context, msg sqstypes.Message) {
	if msg.Body == nil {
		c.logger.Warn().Str("message_id", aws.ToString(msg.MessageId)).Msg("Received message with nil body, skipping")
		return
	}

	var o orderdomain.OrderRequest
	if err := json.Unmarshal([]byte(*msg.Body), &o); err != nil {
		c.logger.Error().
			Err(err).
			Str("message_id", aws.ToString(msg.MessageId)).
			Msg("Failed to unmarshal order from SQS message, skipping")
		return
	}

	c.logger.Info().
		Str("message_id", aws.ToString(msg.MessageId)).
		Int("order_id", o.OrderID).
		Msg("Processing order")

	if err := c.orderService.Handle(o); err != nil {
		c.logger.Error().
			Err(err).
			Str("message_id", aws.ToString(msg.MessageId)).
			Int("order_id", o.OrderID).
			Msg("Failed to handle order, leaving on queue for retry")
		return
	}

	// Delete message only after successful handling
	if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	}); err != nil {
		c.logger.Error().
			Err(err).
			Str("message_id", aws.ToString(msg.MessageId)).
			Msg("Failed to delete SQS message")
		return
	}

	c.logger.Info().
		Str("message_id", aws.ToString(msg.MessageId)).
		Int("order_id", o.OrderID).
		Msg("Order processed and message deleted from queue")
}
