package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"webweave/core"
)

func main() {
	if err := run(); err != nil {
		slog.Error("WebWeave 启动或运行失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	host := flag.String("host", "127.0.0.1", "本地监听地址")
	port := flag.Int("port", 8765, "HTTP 监听端口")
	root := flag.String("root", ".", "包含 config.json、webui 和 data 的工作空间")
	cli := flag.Bool("cli", false, "运行命令行对话")
	runID := flag.String("run-id", "", "命令行对话恢复的运行 ID")
	flag.Parse()
	if *host != "127.0.0.1" {
		return errors.New("WebUI 只允许监听 127.0.0.1")
	}
	if *port < 0 || *port > 65535 {
		return errors.New("port 必须在 0 到 65535 之间")
	}
	app, err := core.New(*root)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *cli {
		return app.Chat(ctx, os.Stdin, os.Stdout, *runID)
	}
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", *host, *port), Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP 服务停止失败", "error", err)
		}
	}()
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	slog.Info("WebWeave WebUI 已启动", "url", "http://"+listener.Addr().String())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
