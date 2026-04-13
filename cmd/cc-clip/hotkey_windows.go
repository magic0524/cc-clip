//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modWin      = 0x0008
	modNoRepeat = 0x4000
	wmHotkey    = 0x0312
)

const defaultHotkeyString = "alt+shift+v"

var hotkeyRunning atomic.Bool

type hotkeyBinding struct {
	modifiers uint32
	key       uint32
	display   string
}

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

func cmdHotkey() {
	storedCfg, hasStoredCfg, err := loadHotkeyConfig()
	if err != nil {
		log.Fatalf("hotkey config error: %v", err)
	}
	if !hasStoredCfg {
		storedCfg = hotkeyConfig{
			RemoteDir: defaultRemoteUploadDir,
			DelayMS:   150,
		}
	}

	// Collect all leading non-flag arguments as hosts.
	var hosts []string
	flagArgs := os.Args[2:]
	for i, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, "-") {
			flagArgs = os.Args[2+i:]
			break
		}
		hosts = append(hosts, arg)
		flagArgs = os.Args[2+i+1:]
	}

	fs := flag.NewFlagSet("hotkey", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	remoteDir := fs.String("remote-dir", storedCfg.RemoteDir, "remote upload directory")
	delayMS := fs.Int("delay-ms", storedCfg.DelayMS, "delay before Ctrl+Shift+V after the hotkey")
	hotkeyValue := fs.String("hotkey", storedCfg.Hotkey, "global hotkey to trigger remote paste (default: alt+shift+v)")
	stop := fs.Bool("stop", false, "stop the background hotkey process")
	status := fs.Bool("status", false, "show hotkey status")
	enableAutostart := fs.Bool("enable-autostart", false, "start the hotkey automatically at login")
	disableAutostart := fs.Bool("disable-autostart", false, "remove hotkey auto-start at login")
	runLoop := fs.Bool("run-loop", false, "internal background loop")

	if err := fs.Parse(flagArgs); err != nil {
		log.Fatal(err)
	}

	if *delayMS < 0 {
		log.Fatalf("invalid --delay-ms: %d", *delayMS)
	}

	if *stop {
		stopHotkeyProcess()
		return
	}
	if *disableAutostart {
		if err := uninstallHotkeyAutostart(); err != nil {
			log.Fatalf("failed to disable hotkey auto-start: %v", err)
		}
		stopHotkeyProcess()
		fmt.Println("hotkey: auto-start disabled")
		return
	}
	if *status {
		printHotkeyStatus()
		return
	}

	if len(hosts) == 0 {
		hosts = storedCfg.Hosts
	}
	if len(hosts) == 0 {
		log.Fatal("usage: cc-clip hotkey <host> [<host2> ...] [--remote-dir DIR] [--hotkey KEY] [--delay-ms N] [--enable-autostart] [--disable-autostart] [--stop] [--status]")
	}

	cfg := hotkeyConfig{
		Hosts:     hosts,
		RemoteDir: *remoteDir,
		DelayMS:   *delayMS,
		Hotkey:    *hotkeyValue,
	}
	normalizeHotkeyConfig(&cfg)
	binding, err := parseHotkey(cfg.Hotkey)
	if err != nil {
		log.Fatalf("failed to parse hotkey: %v", err)
	}
	cfg.Hotkey = binding.String()
	if err := saveHotkeyConfig(cfg); err != nil {
		log.Fatalf("failed to save hotkey config: %v", err)
	}

	if *enableAutostart {
		if err := installHotkeyAutostart(); err != nil {
			log.Fatalf("failed to enable hotkey auto-start: %v", err)
		}
		fmt.Println("hotkey: auto-start enabled")
	}

	if *runLoop {
		runHotkeyLoop(cfg.Hosts, cfg.RemoteDir, cfg.Hotkey, time.Duration(cfg.DelayMS)*time.Millisecond)
		return
	}

	startHotkeyBackground(cfg.Hosts, cfg.RemoteDir, cfg.Hotkey, cfg.DelayMS)
}

func startHotkeyBackground(hosts []string, remoteDir, hotkey string, delayMS int) {
	hotkeyStopIfStale()
	if pid, ok := hotkeyProcessPID(); ok {
		fmt.Printf("hotkey: already running (PID %d)\n", pid)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot determine executable path: %v", err)
	}

	args := append([]string{"hotkey"}, hosts...)
	args = append(args,
		"--remote-dir", remoteDir,
		"--hotkey", hotkey,
		"--delay-ms", strconv.Itoa(delayMS),
		"--run-loop",
	)
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start hotkey process: %v", err)
	}

	if err := writeHotkeyPID(cmd.Process.Pid); err != nil {
		log.Fatalf("hotkey started (PID %d) but pid file write failed: %v", cmd.Process.Pid, err)
	}
	fmt.Printf("hotkey: started in background (PID %d), trigger with %s\n", cmd.Process.Pid, hotkey)
}

func runHotkeyLoop(hosts []string, remoteDir, hotkey string, delay time.Duration) {
	if err := initHotkeyLogger(); err != nil {
		log.Fatalf("hotkey logger setup failed: %v", err)
	}
	if err := writeHotkeyPID(os.Getpid()); err != nil {
		log.Fatalf("hotkey pid file write failed: %v", err)
	}
	defer os.Remove(hotkeyPIDPath())

	// Remove stale stop file only if it predates our startup. This avoids a
	// TOCTOU race where --stop writes the sentinel between VBS respawn and
	// this cleanup line, which would cause the sentinel to be deleted and the
	// VBS loop to restart us again.
	if info, err := os.Stat(hotkeyStopFilePath()); err == nil {
		if info.ModTime().Before(time.Now().Add(-2 * time.Second)) {
			os.Remove(hotkeyStopFilePath())
		}
	}

	binding, err := parseHotkey(hotkey)
	if err != nil {
		log.Fatalf("hotkey: invalid hotkey %q: %v", hotkey, err)
	}

	cfg := hotkeyConfig{
		Hosts:     hosts,
		RemoteDir: remoteDir,
		DelayMS:   int(delay / time.Millisecond),
		Hotkey:    hotkey,
	}

	log.Printf("hotkey: starting for hosts=%s remote_dir=%s hotkey=%s", strings.Join(hosts, ","), remoteDir, binding.String())

	// Create tray (this also calls runtime.LockOSThread)
	tray, err := newTray(cfg, binding, defaultDaemonPort())
	if err != nil {
		log.Printf("hotkey: tray creation failed (continuing without tray): %v", err)
	}

	var trayHwnd uintptr
	if tray != nil {
		if err := tray.show(); err != nil {
			log.Printf("hotkey: tray show failed: %v", err)
		} else {
			trayHwnd = tray.hwnd
			defer tray.remove()
		}
	}

	user32 := syscall.NewLazyDLL("user32.dll")
	registerHotKey := user32.NewProc("RegisterHotKey")
	unregisterHotKey := user32.NewProc("UnregisterHotKey")
	getMessage := user32.NewProc("GetMessageW")
	translateMessage := user32.NewProc("TranslateMessage")
	dispatchMessage := user32.NewProc("DispatchMessageW")

	const hotkeyID = 1
	r1, _, regErr := registerHotKey.Call(trayHwnd, hotkeyID, uintptr(binding.modifiers|modNoRepeat), uintptr(binding.key))
	if r1 == 0 {
		log.Fatalf("hotkey: RegisterHotKey failed: %v", regErr)
	}
	defer unregisterHotKey.Call(trayHwnd, hotkeyID)
	log.Printf("hotkey: registered %s", binding.String())

	var m msg
	for {
		ret, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(ret) {
		case -1:
			log.Printf("hotkey: GetMessageW returned error")
			return
		case 0:
			log.Printf("hotkey: message loop exited")
			return
		}

		// When tray is absent (trayHwnd == 0), WM_HOTKEY is posted to the
		// thread message queue and DispatchMessage won't route it anywhere.
		// Handle it explicitly here so the hotkey works in tray-less mode.
		if m.message == wmHotkey && tray == nil {
			if !hotkeyRunning.Swap(true) {
				go func() {
					defer hotkeyRunning.Store(false)
					if err := handleHotkeyPress(hosts, remoteDir, binding, delay); err != nil {
						log.Printf("hotkey: send failed: %v", err)
						return
					}
					log.Printf("hotkey: send completed")
				}()
			}
			continue
		}

		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func handleHotkeyPress(hosts []string, remoteDir string, binding hotkeyBinding, delay time.Duration) error {
	log.Printf("hotkey: %s pressed", binding.String())

	tray := globalTray
	if tray != nil {
		tray.showBalloon("cc-clip", "Uploading clipboard image...", niifInfo)
	}

	// Read clipboard once; fan-out to all hosts from the same temp file.
	localPath, ext, err := readClipboardToTempFile()
	if err != nil {
		if tray != nil {
			if strings.Contains(err.Error(), "no image in clipboard") {
				tray.showBalloon("cc-clip", "No image in clipboard", niifWarning)
			} else {
				tray.showBalloon("cc-clip", "Send failed: "+err.Error(), niifError)
			}
		}
		return err
	}
	defer os.Remove(localPath)
	_ = ext // extension already embedded in the filename chosen by readClipboardToTempFile

	type hostResult struct {
		host       string
		remotePath string
		err        error
	}
	results := make([]hostResult, len(hosts))

	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			res, err := uploadImage(h, remoteDir, localPath)
			if err != nil {
				results[idx] = hostResult{host: h, err: err}
				log.Printf("hotkey: upload to %s failed: %v", h, err)
				return
			}
			results[idx] = hostResult{host: h, remotePath: res.RemotePath}
			log.Printf("hotkey: uploaded %s to %s", res.RemotePath, h)
		}(i, host)
	}
	wg.Wait()

	// Find first successful result for paste.
	var firstRemotePath, firstHost string
	successCount := 0
	for _, r := range results {
		if r.err == nil {
			successCount++
			if firstRemotePath == "" {
				firstRemotePath = r.remotePath
				firstHost = r.host
			}
		}
	}

	if successCount == 0 {
		// All hosts failed; surface the first error.
		firstErr := results[0].err
		if tray != nil {
			tray.showBalloon("cc-clip", "Send failed: "+firstErr.Error(), niifError)
		}
		return firstErr
	}

	if err := pasteRemotePath(firstRemotePath, localPath, delay, true); err != nil {
		if tray != nil {
			tray.showBalloon("cc-clip", "Paste failed: "+err.Error(), niifError)
		}
		return err
	}

	if tray != nil {
		msg := fmt.Sprintf("Image pasted to %s (%d/%d hosts)", firstHost, successCount, len(hosts))
		tray.showBalloon("cc-clip", msg, niifInfo)
	}
	return nil
}

func printHotkeyStatus() {
	pid, ok := hotkeyProcessPID()
	if !ok {
		fmt.Println("hotkey: not running")
	} else {
		fmt.Printf("hotkey: running (PID %d)\n", pid)
	}

	if hotkeyAutostartEnabled() {
		fmt.Println("hotkey: auto-start enabled")
	} else {
		fmt.Println("hotkey: auto-start disabled")
	}

	cfg, ok, err := loadHotkeyConfig()
	if err != nil {
		fmt.Printf("hotkey: config error: %v\n", err)
		return
	}
	if !ok || len(cfg.Hosts) == 0 {
		fmt.Println("hotkey: no saved default hosts")
		return
	}

	for i, h := range cfg.Hosts {
		fmt.Printf("hotkey: host[%d] %s\n", i, h)
	}
	fmt.Printf("hotkey: remote dir %s\n", cfg.RemoteDir)
	fmt.Printf("hotkey: delay %dms\n", cfg.DelayMS)
	fmt.Printf("hotkey: key %s\n", cfg.Hotkey)
}

func stopHotkeyProcess() {
	// Write stop sentinel unconditionally so the VBS autostart loop exits
	// even if the hotkey process has crashed and the PID file is gone.
	// The sentinel is harmless if no VBS loop is running — it gets cleaned
	// up on the next --run-loop start.
	writeHotkeyStopFile()

	pid, ok := hotkeyProcessPID()
	if !ok {
		fmt.Println("hotkey: not running (stop sentinel written)")
		return
	}

	cmdline, err := localProcessCommand(pid)
	if err == nil && !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		fmt.Printf("hotkey: pid %d is not a cc-clip hotkey process, refusing to kill\n", pid)
		os.Remove(hotkeyPIDPath())
		return
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Println("hotkey: process not found")
		os.Remove(hotkeyPIDPath())
		return
	}
	_ = proc.Kill()
	os.Remove(hotkeyPIDPath())
	fmt.Printf("hotkey: stopped PID %d\n", pid)
}

func hotkeyStopIfStale() {
	pid, ok := hotkeyProcessPID()
	if !ok {
		return
	}
	cmdline, err := localProcessCommand(pid)
	if err != nil || !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		os.Remove(hotkeyPIDPath())
	}
}

func hotkeyProcessPID() (int, bool) {
	data, err := os.ReadFile(hotkeyPIDPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(hotkeyPIDPath())
		return 0, false
	}
	cmdline, err := localProcessCommand(pid)
	if err != nil || !strings.Contains(strings.ToLower(cmdline), " hotkey ") {
		_ = os.Remove(hotkeyPIDPath())
		return 0, false
	}
	return pid, true
}

var hotkeyPIDPathOverride string

func hotkeyPIDPath() string {
	if hotkeyPIDPathOverride != "" {
		return hotkeyPIDPathOverride
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheDir, "cc-clip", "hotkey.pid")
}

func hotkeyLogPath() string {
	return hotkeyLogPathFunc()
}

var hotkeyLogPathFunc = func() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheDir, "cc-clip", "hotkey.log")
}

func writeHotkeyPID(pid int) error {
	if err := os.MkdirAll(filepath.Dir(hotkeyPIDPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(hotkeyPIDPath(), []byte(strconv.Itoa(pid)), 0644)
}

func initHotkeyLogger() error {
	if err := os.MkdirAll(filepath.Dir(hotkeyLogPath()), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(hotkeyLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags)
	return nil
}

func parseHotkey(value string) (hotkeyBinding, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		value = defaultHotkeyString
	}

	parts := strings.Split(value, "+")
	if len(parts) < 2 {
		return hotkeyBinding{}, fmt.Errorf("expected at least one modifier and one key, got %q", value)
	}

	var modifiers uint32
	var keyToken string
	seen := map[string]bool{}
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			return hotkeyBinding{}, fmt.Errorf("invalid hotkey %q", value)
		}
		if seen[token] {
			return hotkeyBinding{}, fmt.Errorf("duplicate hotkey token %q", token)
		}
		seen[token] = true

		switch token {
		case "alt":
			modifiers |= modAlt
		case "ctrl", "control":
			modifiers |= modControl
		case "shift":
			modifiers |= modShift
		case "win", "windows", "meta":
			modifiers |= modWin
		default:
			if keyToken != "" {
				return hotkeyBinding{}, fmt.Errorf("multiple keys in hotkey %q", value)
			}
			keyToken = token
		}
	}

	if modifiers == 0 {
		return hotkeyBinding{}, fmt.Errorf("hotkey %q must include at least one modifier", value)
	}
	if keyToken == "" {
		return hotkeyBinding{}, fmt.Errorf("hotkey %q must include a key", value)
	}

	key, displayKey, err := parseHotkeyKey(keyToken)
	if err != nil {
		return hotkeyBinding{}, err
	}

	displayParts := make([]string, 0, 4)
	if modifiers&modControl != 0 {
		displayParts = append(displayParts, "ctrl")
	}
	if modifiers&modAlt != 0 {
		displayParts = append(displayParts, "alt")
	}
	if modifiers&modShift != 0 {
		displayParts = append(displayParts, "shift")
	}
	if modifiers&modWin != 0 {
		displayParts = append(displayParts, "win")
	}
	displayParts = append(displayParts, displayKey)

	return hotkeyBinding{
		modifiers: modifiers,
		key:       key,
		display:   strings.Join(displayParts, "+"),
	}, nil
}

func parseHotkeyKey(token string) (uint32, string, error) {
	if len(token) == 1 {
		ch := token[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return uint32(ch - 'a' + 'A'), token, nil
		case ch >= '0' && ch <= '9':
			return uint32(ch), token, nil
		}
	}

	if strings.HasPrefix(token, "f") {
		n, err := strconv.Atoi(strings.TrimPrefix(token, "f"))
		if err == nil && n >= 1 && n <= 24 {
			return uint32(0x70 + n - 1), token, nil
		}
	}

	special := map[string]struct {
		key     uint32
		display string
	}{
		"insert": {0x2D, "insert"},
		"delete": {0x2E, "delete"},
	}
	if entry, ok := special[token]; ok {
		return entry.key, entry.display, nil
	}

	return 0, "", fmt.Errorf("unsupported hotkey key %q", token)
}

func (h hotkeyBinding) String() string {
	return h.display
}
