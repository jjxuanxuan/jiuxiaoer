package dispatch

import (
	"context"
	"encoding/json"
	"strconv"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/mq"
)

type MQHandler struct{ service *Service }

// NewMQHandler 创建并初始化消息队列处理器。
func NewMQHandler(service *Service) *MQHandler { return &MQHandler{service: service} }

// Handle 处理消费者结果请求。
func (h *MQHandler) Handle(ctx context.Context, _ *gorm.DB, envelope mq.EventEnvelope) (mq.ConsumerResult, error) {
	if h == nil || h.service == nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("DISPATCH_WORKER_UNAVAILABLE", "dispatch worker is unavailable", nil)
	}
	if envelope.EventType == "dispatch.policy.published" {
		return mq.ConsumerResult{RefType: "dispatch_policy"}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("DISPATCH_PAYLOAD_INVALID", "dispatch event payload is invalid", err)
	}
	jobID := payloadUint(payload, "dispatch_job_id")
	if jobID == 0 {
		return mq.ConsumerResult{}, mq.TerminalConsumerError("DISPATCH_JOB_ID_INVALID", "dispatch job id is invalid", nil)
	}
	if err := h.service.ProcessJobID(ctx, jobID); err != nil {
		return mq.ConsumerResult{}, mq.TemporaryConsumerError("DISPATCH_JOB_PROCESS_FAILED", "dispatch job processing failed", err)
	}
	return mq.ConsumerResult{RefType: "dispatch_job", RefID: jobID}, nil
}

// payloadUint 返回载荷 Uint。
func payloadUint(payload map[string]any, key string) uint64 {
	switch value := payload[key].(type) {
	case string:
		id, _ := strconv.ParseUint(value, 10, 64)
		return id
	case float64:
		if value > 0 {
			return uint64(value)
		}
	}
	return 0
}
