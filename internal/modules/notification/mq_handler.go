package notification

import (
	"context"
	"strconv"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/mq"
)

type MQHandler struct {
	worker *Worker
}

// NewMQHandler 创建并初始化消息队列处理器。
func NewMQHandler(worker *Worker) *MQHandler { return &MQHandler{worker: worker} }

// Handle 处理消费者结果请求。
func (h *MQHandler) Handle(ctx context.Context, tx *gorm.DB, envelope mq.EventEnvelope) (mq.ConsumerResult, error) {
	if h == nil || h.worker == nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("NOTIFICATION_WORKER_UNAVAILABLE", "notification worker is unavailable", nil)
	}
	aggregateID, err := strconv.ParseUint(envelope.AggregateID, 10, 64)
	if err != nil {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("NOTIFICATION_AGGREGATE_INVALID", "notification aggregate is invalid", err)
	}
	if err := h.worker.MaterializeEvent(ctx, tx, envelope.EventID, envelope.EventType, envelope.AggregateType, aggregateID, datatypes.JSON(envelope.Payload), envelope.OccurredAt); err != nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("NOTIFICATION_MATERIALIZE_FAILED", "notification materialization failed", err)
	}
	return mq.ConsumerResult{RefType: "notification"}, nil
}
