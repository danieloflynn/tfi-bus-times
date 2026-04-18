package updater

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultTarget  = "/usr/local/bin/tfi-display"
	defaultService = "tfi-display"
	defaultTimeout = 30 * time.Second
	binaryName     = "tfi-display"
)

// Config controls updater behaviour.
type Config struct {
	// StagingDir is where the new tfi-display binary is expected.
	// Defaults to the directory containing the running updater executable.
	StagingDir string
	// TargetBinary is the absolute install path.
	TargetBinary string
	// ServiceName is the systemd service to restart.
	ServiceName string
	// WaitTimeout is how long to wait for the service to become active.
	WaitTimeout time.Duration
}

// DefaultConfig returns a Config with all defaults populated.
// StagingDir is set to the directory of the running updater binary.
func DefaultConfig() (Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("resolving executable path: %w", err)
	}
	return Config{
		StagingDir:   filepath.Dir(exe),
		TargetBinary: defaultTarget,
		ServiceName:  defaultService,
		WaitTimeout:  defaultTimeout,
	}, nil
}

// systemctlCmd is a package-level variable so tests can substitute a fake.
var systemctlCmd = func(args ...string) *exec.Cmd {
	return exec.Command("systemctl", args...)
}

// Run executes the full update lifecycle:
// find staged binary → backup existing → install → restart service → verify → rollback on failure.
func Run(cfg Config) error {
	staged, err := findStagedBinary(cfg.StagingDir, binaryName)
	if err != nil {
		return fmt.Errorf("finding staged binary: %w", err)
	}
	log.Printf("staged binary: %s", staged)

	if err := backupBinary(cfg.TargetBinary); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	if err := installBinary(staged, cfg.TargetBinary); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	log.Printf("installed %s → %s", staged, cfg.TargetBinary)

	if err := restartService(cfg.ServiceName); err != nil {
		log.Printf("restart failed: %v — rolling back", err)
		if rbErr := rollback(cfg.TargetBinary, cfg.ServiceName); rbErr != nil {
			return fmt.Errorf("rollback after restart failure: %w (restart error: %v)", rbErr, err)
		}
		return fmt.Errorf("restart: %w", err)
	}

	if err := waitForActive(cfg.ServiceName, cfg.WaitTimeout); err != nil {
		log.Printf("service not healthy after update: %v — rolling back", err)
		if rbErr := rollback(cfg.TargetBinary, cfg.ServiceName); rbErr != nil {
			return fmt.Errorf("rollback after failed start: %w (original: %v)", rbErr, err)
		}
		return fmt.Errorf("service failed to become active after update: %w", err)
	}

	log.Printf("service %s is active — update complete", cfg.ServiceName)
	return nil
}

// findStagedBinary returns the full path to <name> in stagingDir, or an error if absent.
func findStagedBinary(stagingDir, name string) (string, error) {
	p := filepath.Join(stagingDir, name)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s not found in staging dir %s", name, stagingDir)
		}
		return "", fmt.Errorf("stat %s: %w", p, err)
	}
	return p, nil
}

// backupBinary copies dst → dst+".prev". No-op if dst does not exist.
func backupBinary(dst string) error {
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return nil
	}
	return copyFile(dst, dst+".prev")
}

// installBinary copies src to dst atomically via a ".new" staging file.
func installBinary(src, dst string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dst, err)
	}
	return nil
}

// rollback restores dst from dst+".prev" and restarts the service.
func rollback(dst, serviceName string) error {
	prev := dst + ".prev"
	if _, err := os.Stat(prev); os.IsNotExist(err) {
		return fmt.Errorf("no backup at %s to roll back to", prev)
	}
	if err := copyFile(prev, dst); err != nil {
		return fmt.Errorf("restoring backup: %w", err)
	}
	log.Printf("rolled back %s from %s", dst, prev)
	return restartService(serviceName)
}

// restartService runs systemctl restart <name>.
func restartService(name string) error {
	cmd := systemctlCmd("restart", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s: %w\n%s", name, err, out)
	}
	return nil
}

// waitForActive polls systemctl is-active until the service reports "active"
// or timeout elapses.
func waitForActive(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := systemctlCmd("is-active", name)
		out, _ := cmd.Output()
		if strings.TrimSpace(string(out)) == "active" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %s to become active", name)
}

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	return nil
}
