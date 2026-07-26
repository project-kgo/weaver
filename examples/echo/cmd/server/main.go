package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/project-kgo/weaver"
	"github.com/project-kgo/weaver/examples/echo/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	unit := flag.String("unit", os.Getenv("APP_UNIT"), "当前部署单元")
	configPath := flag.String("config", "config/monolith.yaml", "Weaver YAML 配置")
	listenAddress := flag.String("listen", ":8080", "HTTP 监听地址")
	prefix := flag.String("prefix", "echo:", "示例前缀")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	config, err := weaver.ParseConfig(data)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := weaver.New(
		ctx,
		*unit,
		config,
		weaver.WithResource(&app.Settings{Prefix: *prefix}),
	)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           runtime.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveError := make(chan error, 1)
	go func() {
		serveError <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-serveError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(server.Shutdown(shutdownCtx), runtime.Shutdown(shutdownCtx))
}
