package printjob

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/mq"
)

type MQHandler struct {
	worker *Worker
}

type printWakeupPayload struct {
	PrintTaskID string `json:"print_task_id"`
}

// NewMQHandler 创建并初始化消息队列 Handler。
func NewMQHandler(worker *Worker) *MQHandler { return &MQHandler{worker: worker} }

// Handle 处理消费者结果请求。
func (h *MQHandler) Handle(ctx context.Context, tx *gorm.DB, envelope mq.EventEnvelope) (mq.ConsumerResult, error) {
	var payload printWakeupPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("PRINT_WAKEUP_PAYLOAD_INVALID", "print wakeup payload is invalid", err)
	}
	taskID, err := parseID(payload.PrintTaskID)
	if err != nil || taskID == 0 {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("PRINT_TASK_ID_INVALID", "print task id is invalid", err)
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&Task{}).Where("id=?", taskID).Count(&count).Error; err != nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("PRINT_TASK_LOOKUP_FAILED", "print task lookup failed", err)
	}
	if count == 0 {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("PRINT_TASK_NOT_FOUND", "print task does not exist", nil)
	}
	return mq.ConsumerResult{RefType: "print_task", RefID: taskID}, nil
}

// AfterCommit 返回售后 Commit。
func (h *MQHandler) AfterCommit(ctx context.Context, _ mq.EventEnvelope, result mq.ConsumerResult) error {
	if h == nil || h.worker == nil || result.RefID == 0 {
		return nil
	}
	return h.worker.RunTask(ctx, result.RefID)
}
