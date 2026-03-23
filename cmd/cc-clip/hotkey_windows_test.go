//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestStopHotkeyProcessWritesStopSentinel(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "hotkey.pid")
	stopFile := filepath.Join(tmpDir, "hotkey.stop")

	hotkeyPIDPathOverride = pidFile
	hotkeyStopFilePathOverride = stopFile
	originalCmdFunc := localProcessCommandFunc
	t.Cleanup(func() {
		hotkeyPIDPathOverride = ""
		hotkeyStopFilePathOverride = ""
		localProcessCommandFunc = originalCmdFunc
	})

	// Mock localProcessCommand so it always reports "hotkey" in the
	// command line — prevents stopHotkeyProcess from refusing to kill.
	localProcessCommandFunc = func(pid int) (string, error) {
		return "cc-clip.exe hotkey --run-loop", nil
	}

	// Start a real child process that stopHotkeyProcess can kill.
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	stopHotkeyProcess()

	// Stop sentinel must exist — this is what prevents the VBS loop from respawning.
	if _, err := os.Stat(stopFile); os.IsNotExist(err) {
		t.Fatal("expected stop sentinel file to be created, but it does not exist")
	}
	// PID file must be cleaned up.
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatal("expected PID file to be removed after stop")
	}
}

func TestStopHotkeyProcessNoopWhenNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "hotkey.pid")
	stopFile := filepath.Join(tmpDir, "hotkey.stop")

	hotkeyPIDPathOverride = pidFile
	hotkeyStopFilePathOverride = stopFile
	originalCmdFunc := localProcessCommandFunc
	t.Cleanup(func() {
		hotkeyPIDPathOverride = ""
		hotkeyStopFilePathOverride = ""
		localProcessCommandFunc = originalCmdFunc
	})

	localProcessCommandFunc = func(pid int) (string, error) {
		return "cc-clip.exe hotkey --run-loop", nil
	}

	// No PID file exists — stopHotkeyProcess should be a no-op.
	stopHotkeyProcess()

	// Stop sentinel must NOT be created when nothing was running,
	// otherwise a future manual start would immediately see the sentinel and exit.
	if _, err := os.Stat(stopFile); !os.IsNotExist(err) {
		t.Fatal("expected no stop sentinel file when hotkey is not running")
	}
}
