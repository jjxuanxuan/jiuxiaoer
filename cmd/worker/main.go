package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"jiuxiaoer-admin/backend-go/internal/app"
	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
)

// main 作为当前命令的程序入口，完成依赖初始化并启动运行。
func main() {
	defaultRole := os.Getenv("JXE_WORKER_ROLE")
	if defaultRole == "" {
		defaultRole = "all"
	}
	role := flag.String("role", defaultRole, "outbox-publisher|mq-consumer-notification|mq-consumer-print|mq-consumer-cache|mq-consumer-security|mq-consumer-dispatch|mq-dead-sink|search-retention|wine-ticket-maintenance|all")
	flag.Parse()

	cfg := config.Load()
	log := logger.New(cfg.App.Env)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := app.NewServer(ctx, cfg, log)
	if err != nil {
		log.Error("failed to build MQ worker", slog.Any("error", err))
		os.Exit(1)
	}
	if err := server.RunMQWorker(ctx, *role); err != nil {
		log.Error("MQ worker stopped with error", slog.String("role", *role), slog.Any("error", err))
		os.Exit(1)
	}
}
