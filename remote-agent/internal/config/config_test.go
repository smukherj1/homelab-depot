package config

import "testing"

func TestValidateAddr(t *testing.T) {
	valid := []string{"127.0.0.1:50051", "[::1]:50051", "localhost:50051", "0.0.0.0:50051", "[::]:50051", "8.8.8.8:50051"}
	for _, addr := range valid {
		if err := ValidateListenAddr(addr); err != nil {
			t.Fatalf("ValidateListenAddr should accept syntactically complete host:port %q, got error %v", addr, err)
		}
	}
	invalid := []string{"127.0.0.1"}
	for _, addr := range invalid {
		if err := ValidateListenAddr(addr); err == nil {
			t.Fatalf("ValidateListenAddr should reject address %q because it lacks an explicit port", addr)
		}
	}
}

func TestValidateRejectsBadLimits(t *testing.T) {
	cfg := Defaults()
	cfg.MaxUploadChunk = cfg.MaxUploadSize + 1
	if err := Validate(cfg); err == nil {
		t.Fatalf("Validate should reject MaxUploadChunk=%d because it exceeds MaxUploadSize=%d", cfg.MaxUploadChunk, cfg.MaxUploadSize)
	}
}
