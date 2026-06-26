package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -- DefaultConfig --

// TestUpdaterEdge_DefaultConfigSensibleDefaults verifies that DefaultConfig returns
// all fields populated with non-zero production defaults.
func TestUpdaterEdge_DefaultConfigSensibleDefaults(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error: %v", err)
	}
	if cfg.StagingDir == "" {
		t.Error("StagingDir must be non-empty")
	}
	if cfg.TargetBinary != defaultTarget {
		t.Errorf("TargetBinary = %q, want %q", cfg.TargetBinary, defaultTarget)
	}
	if cfg.ServiceName != defaultService {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, defaultService)
	}
	if cfg.WaitTimeout != defaultTimeout {
		t.Errorf("WaitTimeout = %v, want %v", cfg.WaitTimeout, defaultTimeout)
	}
}

// -- rollback --

// TestUpdaterEdge_RollbackNoPrev verifies that rollback returns a descriptive error
// when no .prev backup exists at the target path.
func TestUpdaterEdge_RollbackNoPrev(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "tfi-display")
	// No .prev file — rollback must fail, not panic.
	err := rollback(dst, "tfi-display")
	if err == nil {
		t.Fatal("expected error when no .prev backup exists")
	}
	if !strings.Contains(err.Error(), "no backup") {
		t.Errorf("error should mention 'no backup', got: %v", err)
	}
}

// -- installBinary failure paths --

// TestUpdaterEdge_InstallBinaryMissingSrc verifies that installBinary returns an
// error when the source file does not exist.
func TestUpdaterEdge_InstallBinaryMissingSrc(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist")
	dst := filepath.Join(dir, "tfi-display")
	if err := installBinary(src, dst); err == nil {
		t.Fatal("expected error for missing source file")
	}
}

// TestUpdaterEdge_InstallBinaryUnwritableDst verifies that installBinary returns an
// error when the destination directory is not writable.
func TestUpdaterEdge_InstallBinaryUnwritableDst(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "tfi-display")
	if err := os.WriteFile(src, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	if err := os.Chmod(dstDir, 0555); err != nil {
		t.Fatal(err)
	}
	// Restore write bit so t.TempDir cleanup can delete the directory.
	t.Cleanup(func() { _ = os.Chmod(dstDir, 0755) })

	dst := filepath.Join(dstDir, "tfi-display")
	if err := installBinary(src, dst); err == nil {
		t.Fatal("expected error for unwritable destination directory")
	}
}

// -- Run rollback paths --

// TestUpdaterEdge_RestartFailsRollbackRestores verifies that when the first service
// restart fails after a binary install, Run rolls back to the previous binary and
// issues a second restart as part of the rollback lifecycle.
func TestUpdaterEdge_RestartFailsRollbackRestores(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })

	restartCount := 0
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "restart" {
			restartCount++
			if restartCount == 1 {
				return exec.Command("false") // first restart (post-install) fails
			}
			return exec.Command("true") // rollback restart succeeds
		}
		return exec.Command("echo", "active")
	}

	staging := updaterEdgeStagingWithBinary(t, "v2")
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "tfi-display")
	if err := os.WriteFile(target, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StagingDir:   staging,
		TargetBinary: target,
		ServiceName:  "tfi-display",
		WaitTimeout:  5 * time.Second,
	}
	err := Run(cfg)
	if err == nil {
		t.Fatal("expected error after restart failure")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error should mention 'restart', got: %v", err)
	}

	// Rollback must have restored the original binary content.
	updaterEdgeAssertFileContent(t, target, "v1")

	// Two restarts expected: post-install attempt + rollback restart.
	if restartCount != 2 {
		t.Errorf("expected 2 restart calls (install + rollback), got %d", restartCount)
	}
}

// TestUpdaterEdge_WaitTimeoutRollbackTwiceRestart verifies that a waitForActive
// timeout in Run triggers rollback, restores the previous binary, and issues a
// second restart as part of the rollback.
func TestUpdaterEdge_WaitTimeoutRollbackTwiceRestart(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })

	restartCount := 0
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "restart" {
			restartCount++
			return exec.Command("true")
		}
		// is-active always reports non-active.
		return exec.Command("echo", "inactive")
	}

	staging := updaterEdgeStagingWithBinary(t, "v2")
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "tfi-display")
	if err := os.WriteFile(target, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		StagingDir:   staging,
		TargetBinary: target,
		ServiceName:  "tfi-display",
		// Negative duration → deadline is in the past → waitForActive loop never
		// executes → immediate timeout without the 2 s per-iteration sleep.
		WaitTimeout: time.Duration(-1),
	}
	err := Run(cfg)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Rollback must have restored the previous binary.
	updaterEdgeAssertFileContent(t, target, "v1")

	// Two restarts: one after install, one inside rollback.
	if restartCount != 2 {
		t.Errorf("expected 2 restart calls (install + rollback), got %d", restartCount)
	}
}

// TestUpdaterEdge_RunRollbackFailsNoPrev verifies that Run returns a combined error
// that mentions rollback when restart fails and there is no .prev to restore
// (the target binary did not exist before the update attempt).
func TestUpdaterEdge_RunRollbackFailsNoPrev(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "restart" {
			return exec.Command("false") // always fail
		}
		return exec.Command("echo", "active")
	}

	staging := updaterEdgeStagingWithBinary(t, "v2")
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "tfi-display")
	// target does not exist → backupBinary is a no-op → no .prev is created

	cfg := Config{
		StagingDir:   staging,
		TargetBinary: target,
		ServiceName:  "tfi-display",
		WaitTimeout:  5 * time.Second,
	}
	err := Run(cfg)
	if err == nil {
		t.Fatal("expected error when rollback has no backup")
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("error should mention 'rollback', got: %v", err)
	}
}

// -- ApplyConfig rollback paths --

// TestUpdaterEdge_ApplyConfigRestartFailsRollback verifies that ApplyConfig restores
// the previous config content when the service restart fails immediately after the
// atomic config write.
func TestUpdaterEdge_ApplyConfigRestartFailsRollback(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })

	restartCount := 0
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "restart" {
			restartCount++
			if restartCount == 1 {
				return exec.Command("false") // first restart fails
			}
			return exec.Command("true") // rollback restart succeeds
		}
		return exec.Command("echo", "active")
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("old-config"), 0644); err != nil {
		t.Fatal(err)
	}

	err := ApplyConfig([]byte("new-config"), configPath, "tfi-display", 5*time.Second)
	if err == nil {
		t.Fatal("expected error after restart failure")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error should mention 'restart', got: %v", err)
	}

	// After rollback the config must be restored to its original content.
	updaterEdgeAssertFileContent(t, configPath, "old-config")
}

// TestUpdaterEdge_ApplyConfigRollbackFailsNoPrev verifies that ApplyConfig returns a
// combined error that mentions rollback when restart fails and there is no .prev to
// restore (no prior config file existed before the call).
func TestUpdaterEdge_ApplyConfigRollbackFailsNoPrev(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "restart" {
			return exec.Command("false")
		}
		return exec.Command("echo", "active")
	}

	// configPath does not exist → backupBinary is a no-op → no .prev is created.
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	err := ApplyConfig([]byte("new-config"), configPath, "tfi-display", 5*time.Second)
	if err == nil {
		t.Fatal("expected error when rollback has no .prev")
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("error should mention 'rollback', got: %v", err)
	}
}

// -- ApplyConfig: first-deploy (no prior config file) --

// TestUpdaterEdge_ApplyConfigNoPriorFile verifies that ApplyConfig succeeds on the
// first deploy when the config file does not yet exist (backupBinary is a no-op and
// writeFileAtomic creates the file fresh).
func TestUpdaterEdge_ApplyConfigNoPriorFile(t *testing.T) {
	orig := systemctlCmd
	t.Cleanup(func() { systemctlCmd = orig })
	systemctlCmd = func(args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "is-active" {
			return exec.Command("echo", "active")
		}
		return exec.Command("true")
	}

	// configPath does not exist before the call.
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	if err := ApplyConfig([]byte("fresh-config"), configPath, "tfi-display", 5*time.Second); err != nil {
		t.Fatalf("ApplyConfig on first deploy: %v", err)
	}

	updaterEdgeAssertFileContent(t, configPath, "fresh-config")

	// No .prev should exist — nothing was there to back up.
	if _, err := os.Stat(configPath + ".prev"); !os.IsNotExist(err) {
		t.Error(".prev should not exist when there was no prior config file")
	}
}

// -- Backup file mode (observed behaviour) --

// TestUpdaterEdge_BackupFileMode verifies the file permissions of the .prev backup
// created by backupBinary. copyFile always creates the destination with 0644
// regardless of the source mode — the backup copy is never executable.
// In normal rollback the restored binary opens an EXISTING file (which keeps its
// installed 0755 mode), so the 0644 backup copy mode does not cause a regression.
// However, if an operator tries to run the .prev file directly it will not be
// executable. This test documents the observable behaviour.
func TestUpdaterEdge_BackupFileMode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "tfi-display")

	if err := os.WriteFile(dst, []byte("v1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := backupBinary(dst); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dst + ".prev")
	if err != nil {
		t.Fatal(err)
	}
	// copyFile creates the backup with 0644 — the execute bit is not copied.
	// The .prev file is therefore not directly executable.
	if info.Mode().Perm() != 0644 {
		t.Errorf(".prev mode = %v; expected 0644 (copyFile always creates with 0644)", info.Mode().Perm())
	}
}

// -- Package-level helpers (prefix: updaterEdge) --

// updaterEdgeStagingWithBinary creates a temp staging directory containing a fake
// tfi-display binary with the provided content and returns the directory path.
func updaterEdgeStagingWithBinary(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, binaryName)
	if err := os.WriteFile(p, []byte(content), 0755); err != nil {
		t.Fatalf("updaterEdgeStagingWithBinary: writing staged binary: %v", err)
	}
	return dir
}

// updaterEdgeAssertFileContent reads path and fails the test if its content does not
// equal want.
func updaterEdgeAssertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("updaterEdgeAssertFileContent: reading %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("file %s content = %q, want %q", path, data, want)
	}
}
