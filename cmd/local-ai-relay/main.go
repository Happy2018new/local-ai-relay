package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
	"github.com/Happy2018new/local-ai-relay/internal/relay"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "local-ai-relay:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "config.json", "JSON configuration file")
	logPath := flag.String("log-file", "", "append structured JSON logs to this file")
	check := flag.Bool("check", false, "validate configuration and required environment variables, then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("local-ai-relay", version)
		return nil
	}
	file, err := os.Open(*path)
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	cfg, err := config.Load(file)
	file.Close()
	if err != nil {
		return err
	}
	secrets, err := cfg.Secrets()
	if err != nil {
		return err
	}
	if *check {
		fmt.Println("configuration valid")
		return nil
	}
	var output io.Writer = os.Stderr
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0700); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
		logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer logFile.Close()
		output = io.MultiWriter(os.Stderr, logFile)
	}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	proxy, err := relay.New(cfg, secrets, logger)
	if err != nil {
		return err
	}
	defer proxy.Close()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("relay_started", "version", version, "listen", listener.Addr().String(),
		"max_concurrent", cfg.MaxConcurrent, "max_queue", cfg.MaxQueue)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	logger.Info("relay_stopped")
	return nil
}
