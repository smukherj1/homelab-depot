package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCommitUploadDigestAndAtomicFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("test setup should create existing target to verify atomic upload failure: %v", err)
	}
	_, err := CommitUpload(UploadOptions{Workspace: root, Filename: "file.txt", Mode: 0o644, MaxSize: 100}, bytes.NewBufferString("new"), "bad")
	if err == nil {
		t.Fatal("CommitUpload should reject content whose SHA-256 digest does not match the caller-provided digest")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("test should be able to read original target after failed upload: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("CommitUpload should leave existing target unchanged after digest failure, expected %q, got %q", "old", data)
	}
}

func TestCommitUploadAndMetadata(t *testing.T) {
	root := t.TempDir()
	data := []byte("hello")
	sum := sha256.Sum256(data)
	n, err := CommitUpload(UploadOptions{Workspace: root, Filename: "dir/file.txt", Mode: 0o755, MaxSize: 100}, bytes.NewReader(data), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("CommitUpload should accept valid content, digest, path, and mode, got error %v", err)
	}
	if n != uint64(len(data)) {
		t.Fatalf("CommitUpload should report bytes written equal to input length %d, got %d", len(data), n)
	}
	resp, err := Metadata(root, "dir")
	if err != nil {
		t.Fatalf("Metadata should read directory created by CommitUpload, got error %v", err)
	}
	entries := resp.GetDirectoryMetadata().GetEntries()
	if len(entries) != 1 || entries[0].GetName() != "file.txt" || entries[0].GetMode()&0o777 != 0o755 {
		t.Fatalf("Metadata should return one entry named file.txt with mode 0755, got %#v", entries)
	}
}
