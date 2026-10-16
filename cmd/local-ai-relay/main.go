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
	"github.com/Happy2018new/local-ai-relay/internal/tui"
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
	configure := flag.Bool("tui", false, "open the interactive configuration editor")
	flag.Parse()
	if *showVersion {
		fmt.Println("local-ai-relay", version)
		return nil
	}
	var cfg config.Config
	interactive := *configure || flag.NFlag() == 0 && tui.Available()
	if interactive {
		if *check {
			return errors.New("-tui and -check cannot be used together")
		}
		result, err := tui.Run(*path, *logPath)
		if err != nil {
			return err
		}
		if result.Saved {
			fmt.Println("configuration saved:", *path)
		}
		if !result.Start {
			return nil
		}
		cfg = result.Config
	} else {
		var err error
		cfg, err = config.LoadFile(*path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w; run local-ai-relay -tui to create a configuration", err)
			}
			return err
		}
	}
	secrets, err := cfg.Secrets()
	if err != nil {
		return err
	}
	if *check {
		fmt.Println("configuration valid")
		return nil
	}
	if *logPath == "" {
		*logPath = cfg.LogPath(*path)
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
		if interactive {
			output = logFile
		} else {
			output = io.MultiWriter(os.Stderr, logFile)
		}
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
	var viewError error
	if interactive {
		viewDone := make(chan error, 1)
		go func() { viewDone <- tui.ViewLogs(ctx, *logPath, "运行中 · http://"+listener.Addr().String()) }()
		select {
		case err := <-done:
			stop()
			<-viewDone
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case viewError = <-viewDone:
			stop()
		case <-ctx.Done():
			viewError = <-viewDone
		}
	} else {
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("relay_stopped")
	return viewError
}
