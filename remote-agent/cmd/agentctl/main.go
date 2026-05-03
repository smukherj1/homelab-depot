package main

import (
	"context"
	"os"

	"github.com/smukherj/homelab-depot/remote-agent/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
