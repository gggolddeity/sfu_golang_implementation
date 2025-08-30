package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"rtc_testing/internal/room"
	"syscall"
	"time"
)

func NewLogger() *slog.Logger {
	sh := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: true})

	return slog.New(sh)
}

func main() {
	port := "8081"
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger := NewLogger()
	logger.Info(fmt.Sprintf("Running server on port %s", port))

	srv := room.RunServer(ctx, port, logger)

	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- srv.HttpSrv.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		logger.Error("server error caused", err)
		return
	case <-ctx.Done():
		logger.Info(fmt.Sprintf("Shutting down server on port %s", port))
	}

	cancelCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger.Info("shutting down server")

	srv.HttpSrv.Shutdown(cancelCtx)
}
