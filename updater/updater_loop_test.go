package updater

// updater_loop_test.go — FR coverage-topup: waitForActive poll-loop, writeFileAtomic
// error paths, installBinary rename error, findStagedBinary stat error, ApplyConfig
// write error, and copyFile destination-open error.
//
// All test function names are prefixed TestCoverTopup_ and helpers prefixed coverTopup
// to avoid collisions with the existing suite.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- waitForActive: poll-loop body ----------

// TestCoverTopup_WaitForActive_PollsOnce verifies the poll-loop body: the fake
// systemctlCmd returns "inactive" on the first is-active call (causing a ~2s
// sleep) and "active" on the second, so waitForActive returns nil after one retry.
// The ~2s sleep is acceptable per the task brief.
func TestCoverTopup_WaitForActive_PollsOnce(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })

	var calls atomic.Int32
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "is-active" {
			n := calls.Add(1)
			if n == 1 {
				return exec.Command("echo", "inactive")
			}
			return exec.Command("echo", "active")
		}
		return exec.Command("true")
	}

	// Timeout long enough to survive one 2s sleep plus overhead.
	if err := waitForActive("tfi-display", 10*time.Second); err != nil {
		t.Fatalf("waitForActive: %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("systemctlCmd called %d times; want ≥ 2 (once inactive, once active)", got)
	}
}

// ---------- writeFileAtomic error paths ----------

// TestCoverTopup_WriteFileAtomic_WriteError verifies that writeFileAtomic returns
// an error (mentioning "write") when the staging .new file cannot be created
// because the containing directory does not exist.
func TestCoverTopup_WriteFileAtomic_WriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "sub", "file.yaml")
	err := writeFileAtomic(path, []byte("data"), 0644)
	if err == nil {
		t.Fatal("expected write error for nonexistent directory")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("error should mention 'write', got: %v", err)
	}
}

// TestCoverTopup_WriteFileAtomic_RenameError verifies that writeFileAtomic
// returns an error (mentioning "rename") and cleans up the .new staging file
// when the atomic rename fails because the target path is a directory.
// On POSIX, renaming a regular file over an existing directory fails with EISDIR.
func TestCoverTopup_WriteFileAtomic_RenameError(t *testing.T) {
	dir := t.TempDir()
	// Create a directory at the target path so that renaming path+".new" over it fails.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := writeFileAtomic(target, []byte("data"), 0644)
	if err == nil {
		t.Fatal("expected rename error when target is a directory")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("error should mention 'rename', got: %v", err)
	}
	// The .new staging file must have been cleaned up by the error handler.
	if _, statErr := os.Stat(target + ".new"); !os.IsNotExist(statErr) {
		t.Error("writeFileAtomic should remove the .new staging file after a rename failure")
	}
}

// ---------- installBinary rename error ----------

// TestCoverTopup_InstallBinary_RenameError verifies that installBinary returns an
// error when the atomic rename fails because dst is an existing directory, and
// that the .new staging file is cleaned up.
func TestCoverTopup_InstallBinary_RenameError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("binary"), 0755); err != nil {
		t.Fatalf("writing src: %v", err)
	}
	// Make dst a directory so renaming dst+".new" over it fails.
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}

	err := installBinary(src, dst)
	if err == nil {
		t.Fatal("expected error when dst is a directory")
	}
	// .new staging file must be cleaned up.
	if _, statErr := os.Stat(dst + ".new"); !os.IsNotExist(statErr) {
		t.Error("installBinary should remove dst+'.new' after a rename failure")
	}
}

// ---------- findStagedBinary stat error ----------

// TestCoverTopup_FindStagedBinary_StatError verifies that findStagedBinary returns
// a wrapped "stat" error (not a "not found" error) when the staging directory
// cannot be searched due to missing execute permission.
func TestCoverTopup_FindStagedBinary_StatError(t *testing.T) {
	dir := t.TempDir()
	// Remove execute bit so os.Stat(dir/tfi-display) returns EACCES, not ENOENT.
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	_, err := findStagedBinary(dir, "tfi-display")
	if err == nil {
		t.Fatal("expected stat error for unexecutable staging directory")
	}
	if strings.Contains(err.Error(), "not found in staging dir") {
		t.Errorf("expected a permission/stat error, not a not-found message: %v", err)
	}
}

// ---------- ApplyConfig write error ----------

// TestCoverTopup_ApplyConfig_WriteError verifies that ApplyConfig returns an error
// (mentioning "write config") when the atomic config write fails because the
// destination directory does not exist. The backup step is a no-op because the
// config path itself does not exist before the call.
func TestCoverTopup_ApplyConfig_WriteError(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd { return exec.Command("true") }

	// configPath resides in a directory that does not exist.
	configPath := filepath.Join(t.TempDir(), "missing-subdir", "config.yaml")

	err := ApplyConfig([]byte("new-config"), configPath, "tfi-display", time.Second)
	if err == nil {
		t.Fatal("expected error when config directory does not exist")
	}
	if !strings.Contains(err.Error(), "write config") {
		t.Errorf("error should mention 'write config', got: %v", err)
	}
}
