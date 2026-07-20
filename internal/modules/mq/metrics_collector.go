package mq

import (
	"context"
	"database/sql"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// RegisterMetrics 注册指标。
func RegisterMetrics(db *gorm.DB, rabbit *rabbitmq.Manager, registry *metrics.Registry) {
	if registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample {
		return collectMQMetrics(db, rabbit)
	})
}

// collectMQMetrics 收集消息队列指标。
func collectMQMetrics(db *gorm.DB, rabbit *rabbitmq.Manager) []metrics.Sample {
	samples := make([]metrics.Sample, 0)
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		var rows []struct {
			ConsumerName string       `gorm:"column:consumer_name"`
			Oldest       sql.NullTime `gorm:"column:oldest"`
		}
		_ = db.WithContext(ctx).Table("mq_consumer_receipts").Select("consumer_name, MIN(first_received_at) AS oldest").Where("status='processing'").Group("consumer_name").Scan(&rows).Error
		for _, row := range rows {
			age := float64(0)
			if row.Oldest.Valid {
				age = time.Since(row.Oldest.Time).Seconds()
				if age < 0 {
					age = 0
				}
			}
			samples = append(samples, metrics.Sample{Name: "jxe_mq_consumer_lag_seconds", Help: "Age of the oldest processing receipt.", Type: "gauge", Labels: map[string]string{"consumer": row.ConsumerName}, Value: age})
		}
		var open int64
		_ = db.WithContext(ctx).Table("mq_dead_letters").Where("status='open'").Count(&open).Error
		samples = append(samples, metrics.Sample{Name: "jxe_mq_dead_open", Help: "Open persisted MQ dead letters.", Type: "gauge", Value: float64(open)})
	}
	if rabbit == nil || !rabbit.Healthy() {
		return samples
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	connection, err := rabbit.Connection(ctx)
	if err != nil {
		return samples
	}
	_, managedQueues, complete := VerifyManagedTopology(ctx, rabbit, DefaultTopology())
	if complete {
		for _, queue := range DefaultTopology().Queues {
			inspection, ok := managedQueues[queue.Name]
			if !ok {
				continue
			}
			labels := map[string]string{"queue": queue.Name, "consumer": queue.Consumer}
			oldest := float64(0)
			if inspection.HeadMessageTimestamp > 0 {
				oldest = float64(time.Now().Unix() - inspection.HeadMessageTimestamp)
				if oldest < 0 {
					oldest = 0
				}
			}
			samples = append(samples,
				metrics.Sample{Name: "jxe_mq_queue_ready", Help: "Messages ready in a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: float64(inspection.Ready)},
				metrics.Sample{Name: "jxe_mq_queue_unacked", Help: "Messages delivered but not acknowledged in a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: float64(inspection.Unacknowledged)},
				metrics.Sample{Name: "jxe_mq_queue_oldest_seconds", Help: "Age of the oldest ready message in a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: oldest},
				metrics.Sample{Name: "jxe_mq_queue_consumers", Help: "Consumers attached to a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: float64(inspection.Consumers)},
			)
		}
		return samples
	}
	for _, queue := range DefaultTopology().Queues {
		channel, err := connection.Channel()
		if err != nil {
			break
		}
		inspection, inspectErr := channel.QueueInspect(queue.Name)
		_ = channel.Close()
		if inspectErr != nil {
			continue
		}
		labels := map[string]string{"queue": queue.Name, "consumer": queue.Consumer}
		samples = append(samples,
			metrics.Sample{Name: "jxe_mq_queue_ready", Help: "Messages ready in a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: float64(inspection.Messages)},
			metrics.Sample{Name: "jxe_mq_queue_consumers", Help: "Consumers attached to a declared RabbitMQ queue.", Type: "gauge", Labels: labels, Value: float64(inspection.Consumers)},
		)
	}
	return samples
}
