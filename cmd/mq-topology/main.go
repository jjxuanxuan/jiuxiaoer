package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"jiuxiaoer-admin/backend-go/internal/config"
	rabbitinfra "jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
)

// main 作为当前命令的程序入口，完成依赖初始化并启动运行。
func main() {
	verifyOnly := flag.Bool("verify-only", false, "verify the managed topology without declaring resources")
	flag.Parse()

	ctx := context.Background()
	cfg := config.Load()
	log := logger.New(cfg.App.Env)
	manager, err := rabbitinfra.Open(ctx, cfg.RabbitMQ, log)
	if err != nil || manager == nil {
		fail(log, "open RabbitMQ", err)
	}
	defer manager.Close()

	topology := mq.DefaultTopology()
	if !*verifyOnly {
		connection, connectionErr := manager.Connection(ctx)
		if connectionErr != nil {
			fail(log, "connect RabbitMQ", connectionErr)
		}
		channel, channelErr := connection.Channel()
		if channelErr != nil {
			fail(log, "open RabbitMQ channel", channelErr)
		}
		if declareErr := mq.DeclareTopology(channel, topology); declareErr != nil {
			_ = channel.Close()
			fail(log, "declare RabbitMQ topology", declareErr)
		}
		_ = channel.Close()
	}

	drift, queues, complete := mq.VerifyManagedTopology(ctx, manager, topology)
	if !complete {
		fail(log, "verify RabbitMQ topology", fmt.Errorf("management API snapshot unavailable"))
	}
	if len(drift) != 0 {
		for _, item := range drift {
			log.Error("RabbitMQ topology drift", slog.String("resource", item.Resource), slog.String("name", item.Name), slog.String("problem", item.Problem))
		}
		os.Exit(1)
	}
	log.Info("RabbitMQ topology verified", slog.String("version", topology.Version), slog.Int("queues", len(queues)))
}

// fail 将相关对象标记为失败。
func fail(log *slog.Logger, action string, err error) {
	if err == nil {
		err = fmt.Errorf("unknown error")
	}
	log.Error(action, slog.Any("error", err))
	os.Exit(1)
}
