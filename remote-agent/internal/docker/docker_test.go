package docker

import (
	"context"
	"testing"
	"time"

	"github.com/smukherj/homelab-depot/remote-agent/internal/runner"
)

func TestDockerIntegrationRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !DockerAvailable(ctx, "") {
		t.Skip("Docker is not available")
	}
	work := t.TempDir()
	res, err := New("", "ubuntu:26.04").Run(context.Background(), runner.Request{
		SessionID:      "test",
		Workspace:      work,
		Cmd:            []string{"/bin/sh", "-c", "pwd && echo err >&2"},
		Timeout:        30 * time.Second,
		StdoutCap:      1024,
		StderrCap:      1024,
		ContainerImage: "ubuntu:26.04",
	})
	if err != nil {
		t.Fatalf("Docker runner should execute a simple shell command in ubuntu:26.04, got error %v", err)
	}
	if res.ExitCode != 0 || res.Stdout == "" || res.Stderr == "" {
		t.Fatalf("Docker runner should capture zero exit code plus stdout and stderr, got result %#v", res)
	}
}
