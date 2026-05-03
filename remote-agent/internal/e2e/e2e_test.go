package e2e

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestE2EAgentCtlFlow(t *testing.T) {
	agent := filepath.Join("..", "..", "bin", "agent")
	agentctl := filepath.Join("..", "..", "bin", "agentctl")
	if _, err := os.Stat(agent); err != nil {
		t.Skip("agent binary not built")
	}
	if _, err := os.Stat(agentctl); err != nil {
		t.Skip("agentctl binary not built")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "version").Run(); err != nil {
		t.Skip("Docker is not available")
	}
	workspace := t.TempDir()
	cmd := exec.CommandContext(ctx, agent, "-listen", "127.0.0.1:0", "-workspace-root", workspace, "-command-timeout", "30s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("e2e setup should attach to agent stdout to read listen address: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("e2e setup should start agent binary %q: %v", agent, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("agent should print its listen address on stdout before agentctl commands run")
	}
	addr := scanner.Text()
	input := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(input, []byte("e2e-ok\n"), 0o644); err != nil {
		t.Fatalf("e2e setup should create local upload input file %q: %v", input, err)
	}
	runCtl := func(args ...string) (string, string, error) {
		all := append([]string{"-addr", addr}, args...)
		c := exec.CommandContext(ctx, agentctl, all...)
		var out, errOut bytes.Buffer
		c.Stdout = &out
		c.Stderr = &errOut
		err := c.Run()
		return out.String(), errOut.String(), err
	}
	for _, step := range [][]string{
		{"create-session", "e2e"},
		{"upload", "e2e", input, "in.txt"},
		{"exec", "e2e", "--", "/bin/sh", "-c", "cat in.txt > out.txt"},
		{"metadata", "e2e", "out.txt"},
	} {
		if out, errOut, err := runCtl(step...); err != nil {
			t.Fatalf("agentctl %v should succeed during create/upload/exec/metadata flow, got error %v stdout=%q stderr=%q", step, err, out, errOut)
		}
	}
	output := filepath.Join(t.TempDir(), "out.txt")
	if out, errOut, err := runCtl("download", "e2e", "out.txt", output); err != nil {
		t.Fatalf("agentctl download should retrieve remote out.txt into %q, got error %v stdout=%q stderr=%q", output, err, out, errOut)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("e2e should read downloaded output file %q: %v", output, err)
	}
	if string(got) != "e2e-ok\n" {
		t.Fatalf("downloaded file should match original command output %q, got %q", "e2e-ok\n", got)
	}
	if out, errOut, err := runCtl("complete-session", "e2e"); err != nil {
		t.Fatalf("agentctl complete-session should clean up e2e session, got error %v stdout=%q stderr=%q", err, out, errOut)
	}
}
