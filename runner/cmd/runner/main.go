package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.Info("Runner starting...")
	if err := run(); err != nil {
		slog.Error("Runner failed", "error", err)
		os.Exit(1)
	}
	slog.Info("Runner shutting down.")
}

func run() error {
	return nil
}
