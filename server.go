package weaver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"time"
)

const defaultShutdownTimeout = 10 * time.Second

// Serve 使用默认 HTTP 配置启动当前 unit，并在 ctx 结束或收到退出信号后
// 优雅关闭服务与 Runtime。服务同时支持 HTTP/1、HTTP/2 和明文 h2c。
func Serve(ctx context.Context, currentUnit string, config Config, listenAddress string, values ...Option) error {
	if ctx == nil {
		return fmt.Errorf("weaver: context 不能为空")
	}
	serveCtx, stopSignals := signal.NotifyContext(ctx, shutdownSignals()...)
	defer stopSignals()

	runtime, err := New(serveCtx, currentUnit, config, values...)
	if err != nil {
		return err
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           runtime.Handler(),
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveError := make(chan error, 1)
	go func() {
		serveError <- server.ListenAndServe()
	}()

	select {
	case err = <-serveError:
	case <-serveCtx.Done():
		// 恢复信号的默认行为，让关闭期间的第二次信号可以强制退出。
		stopSignals()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		serverErr := server.Shutdown(shutdownCtx)
		// Shutdown 会关闭监听器，等待 Serve 退出后再关闭组件，避免遗留服务 goroutine。
		err = <-serveError
		slog.Info("server shutdown")
		runtimeErr := runtime.Shutdown(shutdownCtx)
		cancel()
		return errors.Join(normalizeServeError(err), serverErr, runtimeErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	slog.Info("server closed")
	return errors.Join(
		normalizeServeError(err),
		server.Shutdown(shutdownCtx),
		runtime.Shutdown(shutdownCtx),
	)
}

func normalizeServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
