package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	agentpb "github.com/smukherj/homelab-depot/remote-agent/gen/go/proto"
	"github.com/smukherj/homelab-depot/remote-agent/internal/config"
	dockerrunner "github.com/smukherj/homelab-depot/remote-agent/internal/docker"
	"github.com/smukherj/homelab-depot/remote-agent/internal/service"
	"github.com/smukherj/homelab-depot/remote-agent/internal/session"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.FromEnv()
	config.RegisterFlags(flag.CommandLine, &cfg)
	flag.Parse()
	if err := config.Validate(cfg); err != nil {
		log.Fatal(err)
	}
	mgr, err := session.NewManager(cfg.WorkspaceRoot, cfg.SessionIdle)
	if err != nil {
		log.Fatal(err)
	}
	defer mgr.Close()
	stopJanitor := make(chan struct{})
	go mgr.RunJanitor(stopJanitor, cfg.JanitorInterval)
	defer close(stopJanitor)

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	agentpb.RegisterAgentServer(grpcServer, service.New(cfg, mgr, dockerrunner.New("", cfg.DockerImage)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("Server shutting down.")
		grpcServer.GracefulStop()
	}()
	log.Printf("Starting agent on %v.", lis.Addr().String())
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
