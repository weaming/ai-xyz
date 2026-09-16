package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/weaming/ai-xyz/ai-gateway/config"
	"github.com/weaming/ai-xyz/ai-gateway/gateway"
)

func main() {
	configPath := flag.String("config", "config.yaml", "YAML 配置文件路径")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}
	handler, err := gateway.New(cfg, logger)
	if err != nil {
		logger.Error("创建网关失败", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:         cfg.Server.Address,
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	shutdownContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownContext.Done()
		timeout := cfg.Server.ShutdownTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("关闭 HTTP 服务失败", "error", err)
		}
	}()

	logger.Info("ai-gateway 启动", "address", cfg.Server.Address, "routes", len(cfg.Routes))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP 服务退出", "error", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "ai-gateway stopped")
}
