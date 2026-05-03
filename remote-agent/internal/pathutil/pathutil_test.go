package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRelativeRejectsUnsafePaths(t *testing.T) {
	invalid := []string{"", ".", "..", "/abs", "a/../b", "a/./b", "a//b", "a/", `a\b`, "a\x00b"}
	for _, p := range invalid {
		if _, err := ValidateRelative(p); err == nil {
			t.Fatalf("ValidateRelative should reject unsafe workspace path %q, but it succeeded", p)
		}
	}
	got, err := ValidateRelative("dir/file.txt")
	if err != nil {
		t.Fatalf("ValidateRelative should accept normal relative path dir/file.txt, got error %v", err)
	}
	if got != filepath.Join("dir", "file.txt") {
		t.Fatalf("ValidateRelative should return OS-native cleaned path %q, got %q", filepath.Join("dir", "file.txt"), got)
	}
}

func TestResolveExistingRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatalf("test setup should create outside secret file for symlink escape check: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveExisting(root, "link/secret"); err == nil {
		t.Fatal("ResolveExisting should reject symlink target outside the workspace, but it succeeded")
	}
}

func TestSafeMode(t *testing.T) {
	if err := SafeMode(0o755); err != nil {
		t.Fatalf("SafeMode should accept regular permission bits 0755, got error %v", err)
	}
	if err := SafeMode(0o4755); err == nil {
		t.Fatal("SafeMode should reject mode 04755 because setuid is outside regular permission bits")
	}
}
