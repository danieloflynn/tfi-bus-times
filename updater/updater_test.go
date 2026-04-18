package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- findStagedBinary ---

func TestFindStagedBinary_Found(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tfi-display")
	if err := os.WriteFile(p, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := findStagedBinary(dir, "tfi-display")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != p {
		t.Errorf("got %q, want %q", got, p)
	}
}

func TestFindStagedBinary_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := findStagedBinary(dir, "tfi-display")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

// --- backupBinary ---

func TestBackupBinary_Exists(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "tfi-display")
	if err := os.WriteFile(dst, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := backupBinary(dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(dst + ".prev")
	if err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("backup content %q, want %q", data, "original")
	}
}

func TestBackupBinary_NotExists(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "nonexistent")
	if err := backupBinary(dst); err != nil {
		t.Fatalf("missing target should be a no-op, got: %v", err)
	}
}

// --- installBinary ---

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(src, dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst not created: %v", err)
	}
	if string(data) != "new binary" {
		t.Errorf("dst content %q, want %q", data, "new binary")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("mode %v, want 0755", info.Mode().Perm())
	}

	// Temp file must not remain.
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Error(".new temp file was not cleaned up")
	}
}

// --- waitForActive ---

func TestWaitForActive_Success(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		return exec.Command("echo", "active")
	}
	if err := waitForActive("tfi-display", 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForActive_Timeout(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		return exec.Command("echo", "inactive")
	}
	err := waitForActive("tfi-display", 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// --- Run (integration with temp dirs + mock systemctl) ---

func TestRun_HappyPath(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })

	calls := []string{}
	systemctlCmd = func(args ...string) *exec.Cmd {
		calls = append(calls, strings.Join(args, " "))
		if len(args) > 0 && args[0] == "is-active" {
			return exec.Command("echo", "active")
		}
		return exec.Command("true")
	}

	staging := t.TempDir()
	target := filepath.Join(t.TempDir(), "tfi-display")
	if err := os.WriteFile(filepath.Join(staging, "tfi-display"), []byte("v2"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StagingDir:   staging,
		TargetBinary: target,
		ServiceName:  "tfi-display",
		WaitTimeout:  5 * time.Second,
	}
	if err := Run(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("target not installed: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("target content %q, want %q", data, "v2")
	}
}

func TestRun_RollbackOnServiceFailure(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "is-active" {
			return exec.Command("echo", "failed")
		}
		return exec.Command("true")
	}

	staging := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "tfi-display")

	// Existing "v1" binary.
	if err := os.WriteFile(target, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	// Staged "v2".
	if err := os.WriteFile(filepath.Join(staging, "tfi-display"), []byte("v2"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StagingDir:   staging,
		TargetBinary: target,
		ServiceName:  "tfi-display",
		WaitTimeout:  50 * time.Millisecond,
	}
	err := Run(cfg)
	if err == nil {
		t.Fatal("expected error after service failure")
	}

	// After rollback, target must contain the original "v1" content.
	data, err2 := os.ReadFile(target)
	if err2 != nil {
		t.Fatalf("target missing after rollback: %v", err2)
	}
	if string(data) != "v1" {
		t.Errorf("after rollback target content %q, want %q", data, "v1")
	}
}
