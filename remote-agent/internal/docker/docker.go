package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/smukherj/homelab-depot/remote-agent/internal/runner"
)

// Runner executes requests by invoking the Docker CLI.
//
// It is stateless aside from default Binary and Image fields and can be copied.
// Run creates a fresh container for each request and depends on the Docker
// daemon and CLI being available to the current user.
type Runner struct {
	// Binary is the Docker executable path or name. An empty value means
	// "docker".
	Binary string
	// Image is the default container image used when runner.Request does not
	// specify ContainerImage.
	Image string
}

// New returns a Docker-backed Runner with default binary handling.
//
// binary may be empty to use "docker". image may be empty; Run then falls back
// to the request image or the built-in ubuntu:26.04 default. New performs no I/O
// and returns no error.
func New(binary, image string) Runner {
	if binary == "" {
		binary = "docker"
	}
	return Runner{Binary: binary, Image: image}
}

// Run executes req in a new Docker container and waits for it to exit.
//
// req must already be validated by the service: Cmd must be non-empty,
// Workspace must be the trusted session workspace, and output caps and timeouts
// should be positive. Run mounts Workspace at /workspace, applies fixed sandbox
// flags, passes Env through, captures stdout and stderr, and removes the
// container on normal Docker --rm cleanup. It returns a runner.Result for normal
// process exits, runner.ErrTimeout on timeout, runner.ErrCancellation on context
// cancellation, runner.ErrOutputLimit when captured output exceeds caps, or an
// error wrapping runner.ErrDocker for Docker setup/runtime failures.
func (r Runner) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	if r.Binary == "" {
		r.Binary = "docker"
	}
	image := req.ContainerImage
	if image == "" {
		image = r.Image
	}
	if image == "" {
		image = "ubuntu:26.04"
	}
	cidfile, err := os.CreateTemp("", "remote-agent-cid-*")
	if err != nil {
		return runner.Result{}, fmt.Errorf("%w: create cidfile: %v", runner.ErrDocker, err)
	}
	cidPath := cidfile.Name()
	_ = cidfile.Close()
	_ = os.Remove(cidPath)
	defer os.Remove(cidPath)
	args := []string{
		"run", "--rm",
		"--cidfile", cidPath,
		"--network", "bridge",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--memory", "512m",
		"--cpus", "1",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--workdir", "/workspace",
		"--mount", "type=bind,src=" + req.Workspace + ",dst=/workspace",
		"--label", "remote-agent.session=" + req.SessionID,
	}
	for _, env := range req.Env {
		args = append(args, "--env", env.Key+"="+env.Value)
	}
	args = append(args, image)
	args = append(args, req.Cmd...)

	runCtx := ctx
	cancel := func() {}
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.Binary, args...)
	var stdout, stderr cappedBuffer
	stdout.cap = req.StdoutCap
	stderr.cap = req.StderrCap
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		_ = r.removeCID(context.Background(), cidPath)
		return runner.Result{}, runner.ErrTimeout
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		_ = r.removeCID(context.Background(), cidPath)
		return runner.Result{}, runner.ErrCancellation
	}
	if stdout.exceeded || stderr.exceeded {
		return runner.Result{}, runner.ErrOutputLimit
	}
	res := runner.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return runner.Result{}, fmt.Errorf("%w: %v", runner.ErrDocker, err)
}

func (r Runner) removeCID(ctx context.Context, cidPath string) error {
	data, err := os.ReadFile(cidPath)
	if err != nil {
		return err
	}
	cid := strings.TrimSpace(string(data))
	if cid == "" {
		return nil
	}
	return exec.CommandContext(ctx, r.Binary, "rm", "-f", cid).Run()
}

type cappedBuffer struct {
	bytes.Buffer
	cap      int64
	exceeded bool
}

// Write appends p until the configured cap is reached.
//
// It always reports len(p) and nil error so the child process can continue
// writing until Run observes that output exceeded the cap. Bytes beyond the cap
// are discarded and the exceeded flag is set as a side effect.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.cap > 0 && int64(b.Len()+len(p)) > b.cap {
		remaining := int(b.cap) - b.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		b.exceeded = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// DockerAvailable reports whether the Docker server is reachable through the
// configured CLI.
//
// binary may be empty to use "docker". ctx controls command lifetime. The
// function returns true only when "docker version" succeeds; it has no side
// effects beyond invoking the CLI.
func DockerAvailable(ctx context.Context, binary string) bool {
	if binary == "" {
		binary = "docker"
	}
	cmd := exec.CommandContext(ctx, binary, "version", "--format", "{{.Server.Version}}")
	return cmd.Run() == nil
}

// RemoveLabeledContainers force-removes containers labeled for a session.
//
// binary may be empty to use "docker". sessionID is matched against the
// remote-agent.session label and should already be a validated session ID. The
// function returns nil when no containers match, or the Docker CLI error from
// listing or removing containers.
func RemoveLabeledContainers(ctx context.Context, binary, sessionID string) error {
	if binary == "" {
		binary = "docker"
	}
	out, err := exec.CommandContext(ctx, binary, "ps", "-aq", "--filter", "label=remote-agent.session="+sessionID).Output()
	if err != nil {
		return err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	return exec.CommandContext(ctx, binary, args...).Run()
}

// ParseMemoryLimit parses a small Docker-style memory limit string.
//
// s may be a base-10 byte count or a value ending in "m" for mebibytes. It
// returns the byte count or strconv.ParseInt's error. The function has no side
// effects and is intended for tests and local validation helpers.
func ParseMemoryLimit(s string) (int64, error) {
	if strings.HasSuffix(s, "m") {
		v, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		return v << 20, err
	}
	return strconv.ParseInt(s, 10, 64)
}
