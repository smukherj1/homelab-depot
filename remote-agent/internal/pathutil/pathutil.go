package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrInvalidPath reports that a caller-supplied workspace-relative path is
// malformed or resolves outside the workspace.
var ErrInvalidPath = errors.New("invalid relative path")

// ValidateRelative validates a slash-separated path supplied by an API caller.
//
// raw must be a non-empty relative path using '/' separators and must not
// contain NUL bytes, empty segments, ".", "..", absolute prefixes, or
// backslashes. The returned string is the cleaned OS-native relative path. On
// failure the error wraps ErrInvalidPath and no filesystem access occurs.
func ValidateRelative(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.Contains(raw, "\x00") {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("%w: backslash separator", ErrInvalidPath)
	}
	if strings.HasPrefix(raw, "/") || path.IsAbs(raw) {
		return "", fmt.Errorf("%w: absolute path", ErrInvalidPath)
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: bad segment", ErrInvalidPath)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%w: escapes workspace", ErrInvalidPath)
	}
	return filepath.FromSlash(clean), nil
}

// ResolveExisting returns the resolved host path for an existing workspace
// entry.
//
// root is the session workspace and raw must pass ValidateRelative. The target
// must exist. Symlinks are evaluated and the final path must remain inside root.
// The returned path is the resolved host path; errors include ErrInvalidPath,
// os.Stat-style errors, and symlink resolution failures.
func ResolveExisting(root, raw string) (string, error) {
	rel, err := ValidateRelative(raw)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, rel)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	if err := EnsureInside(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// ResolveForCreate returns the host path where a new file may be created.
//
// root is the session workspace and raw must pass ValidateRelative. Existing
// parent directories are resolved through symlinks and must remain inside root;
// the destination file itself is not followed. The returned path may not exist
// yet. Errors include ErrInvalidPath and filesystem errors while checking the
// existing parent.
func ResolveForCreate(root, raw string) (string, error) {
	rel, err := ValidateRelative(raw)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, rel)
	parent := filepath.Dir(target)
	if _, err := os.Stat(parent); err == nil {
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", err
		}
		if err := EnsureInside(root, resolvedParent); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := EnsureInside(root, target); err != nil {
		return "", err
	}
	return target, nil
}

// EnsureInside verifies that target is contained by root after absolute path
// normalization.
//
// Callers should pass already-resolved paths when checking symlink-sensitive
// operations. The function returns nil when target is root or a descendant, or
// an error wrapping ErrInvalidPath when target escapes root. It can also return
// filepath.Abs or filepath.Rel errors.
func EnsureInside(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)) {
		return nil
	}
	return fmt.Errorf("%w: symlink escapes workspace", ErrInvalidPath)
}

// SafeMode validates upload file permission bits.
//
// mode must contain only Unix permission bits in the range 0000 through 0777.
// It returns nil for safe regular-file permissions and an error for file-type,
// setuid, setgid, sticky, device, or other high bits.
func SafeMode(mode uint32) error {
	if mode > 0o777 {
		return errors.New("mode must contain only permission bits")
	}
	return nil
}
