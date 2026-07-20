package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"jiuxiaoer-admin/backend-go/internal/app"
	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/logger"
)

// main 作为当前命令的程序入口，完成依赖初始化并启动运行。
func main() {
	// API 进程只负责装配配置、日志、基础设施和 HTTP Server；
	// 业务模块的生命周期由 internal/app 统一管理。
	cfg := config.Load()
	log := logger.New(cfg.App.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := app.NewServer(ctx, cfg, log)
	if err != nil {
		log.Error("failed to build server", slog.Any("error", err))
		os.Exit(1)
	}

	if err := server.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped with error", slog.Any("error", err))
		os.Exit(1)
	}
}
