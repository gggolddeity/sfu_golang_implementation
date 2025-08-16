package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func NewLogger() *slog.Logger {
	sh := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: true})

	return slog.New(sh)
}

func main() {
	if len(os.Args) < 2 {
		panic("please provide port")
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger := NewLogger()
	logger.Info(fmt.Sprintf("Running server on port %s", os.Args[1]))

	srv := RunServer(os.Args[1], logger)

	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- srv.httpSrv.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		logger.Error("server error caused", err)
		return
	case <-ctx.Done():
		logger.Info(fmt.Sprintf("Shutting down server on port %s", os.Args[1]))
	}

	cancelCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	logger.Info("shutting down server")

	srv.httpSrv.Shutdown(cancelCtx)
}
