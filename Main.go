package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultADBPath             = "/root/platform-tools-34.0.4-arm/platform-tools/adb"
	defaultWebPort             = "10000"
	defaultJarPort             = 17891
	defaultInjectMode          = 1
	defaultDuplicateDropMS     = 90
	defaultWatchdogIntervalSec = 60
	defaultScanTimeoutMS       = 350
	defaultADBTimeout          = 12 * time.Second
	defaultJarTimeout          = 1500 * time.Millisecond
	stateFileName              = "devices.json"
	keyServerDevicePath        = "/data/local/tmp/android-tv-agent.jar"
	nativeKeyServerDevicePath  = "/data/local/tmp/android-tv-agent-linux-arm64"
)

//go:embed android/android-tv-agent.jar
var keyServerJar []byte

//go:embed android/android-tv-agent-linux-arm64
var nativeKeyServer []byte

type Device struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Hostname        string    `json:"hostname"`
	IP              string    `json:"ip"`
	ADBSerial       string    `json:"adb_serial"`
	AndroidSerial   string    `json:"android_serial"`
	Model           string    `json:"model"`
	Product         string    `json:"product"`
	AndroidDevice   string    `json:"android_device"`
	MAC             string    `json:"mac"`
	JarPort         int       `json:"jar_port"`
	InjectMode      int       `json:"inject_mode"`
	DuplicateDropMS int       `json:"duplicate_drop_ms"`
	Remembered      bool      `json:"remembered"`
	ADBOK           bool      `json:"adb_ok"`
	JarOK           bool      `json:"jar_ok"`
	Status          string    `json:"status"`
	LastError       string    `json:"last_error"`
	LastSeen        time.Time `json:"last_seen"`
	LastChecked     time.Time `json:"last_checked"`
	LastInstalled   time.Time `json:"last_installed"`
	Source          []string  `json:"source,omitempty"`
}

type stateFile struct {
	Devices []*Device `json:"devices"`
}

type LogEntry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

type App struct {
	mu               sync.Mutex
	opMu             sync.Mutex
	adbPath          string
	statePath        string
	watchdogInterval time.Duration
	devices          map[string]*Device
	scanResults      []*Device
	logs             []LogEntry
}

type ADBDevice struct {
	Serial string
	State  string
	Detail string
}

func getenv(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envNonNegativeInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func newApp() *App {
	wd, _ := os.Getwd()
	statePath := getenv("STATE_FILE", filepath.Join(wd, stateFileName))
	return &App{
		adbPath:          getenv("ADB_PATH", defaultADBPath),
		statePath:        statePath,
		watchdogInterval: time.Duration(envInt("WATCHDOG_INTERVAL_SEC", defaultWatchdogIntervalSec)) * time.Second,
		devices:          make(map[string]*Device),
	}
}

func (a *App) addLog(level, format string, args ...any) {
	entry := LogEntry{
		Time:  time.Now(),
		Level: level,
		Text:  fmt.Sprintf(format, args...),
	}
	log.Printf("[%s] %s", level, entry.Text)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, entry)
	if len(a.logs) > 500 {
		a.logs = append([]LogEntry(nil), a.logs[len(a.logs)-500:]...)
	}
}

func (a *App) loadState() error {
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, dev := range file.Devices {
		normalizeDevice(dev)
		if dev.ID == "" {
			dev.ID = makeDeviceID(*dev)
		}
		dev.Remembered = true
		copy := *dev
		a.devices[copy.ID] = &copy
	}
	return nil
}

func (a *App) saveState() error {
	a.mu.Lock()
	devices := make([]*Device, 0, len(a.devices))
	for _, dev := range a.devices {
		copy := *dev
		copy.Source = nil
		devices = append(devices, &copy)
	}
	a.mu.Unlock()

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})
	data, err := json.MarshalIndent(stateFile{Devices: devices}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.statePath, data, 0644)
}

func normalizeDevice(dev *Device) {
	dev.IP = strings.TrimSpace(dev.IP)
	dev.ADBSerial = strings.TrimSpace(dev.ADBSerial)
	dev.AndroidSerial = strings.TrimSpace(dev.AndroidSerial)
	dev.Model = strings.TrimSpace(dev.Model)
	dev.Product = strings.TrimSpace(dev.Product)
	dev.AndroidDevice = strings.TrimSpace(dev.AndroidDevice)
	dev.Hostname = strings.TrimSpace(dev.Hostname)
	dev.MAC = strings.ToUpper(strings.TrimSpace(dev.MAC))
	if dev.JarPort == 0 {
		dev.JarPort = defaultJarPort
	}
	if dev.InjectMode == 0 {
		dev.InjectMode = envInt("JAR_INJECT_MODE", defaultInjectMode)
	}
	if dev.DuplicateDropMS == 0 {
		dev.DuplicateDropMS = envNonNegativeInt("JAR_DUPLICATE_DROP_MS", defaultDuplicateDropMS)
	}
	if dev.Name == "" {
		switch {
		case dev.Hostname != "":
			dev.Name = dev.Hostname
		case dev.Model != "":
			dev.Name = dev.Model
		case dev.IP != "":
			dev.Name = dev.IP
		default:
			dev.Name = "Android TV"
		}
	}
	if dev.ID == "" {
		dev.ID = makeDeviceID(*dev)
	}
}

func makeDeviceID(dev Device) string {
	if validIdentity(dev.AndroidSerial) {
		return "serial-" + sanitizeID(dev.AndroidSerial)
	}
	if dev.Hostname != "" && dev.Model != "" {
		return "host-" + sanitizeID(dev.Hostname+"-"+dev.Model)
	}
	if dev.MAC != "" {
		return "mac-" + sanitizeID(dev.MAC)
	}
	if dev.ADBSerial != "" {
		return "adb-" + sanitizeID(dev.ADBSerial)
	}
	if dev.IP != "" {
		return "ip-" + sanitizeID(dev.IP)
	}
	return "device-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func sanitizeID(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return strings.Trim(b.String(), "-")
}

func validIdentity(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return value != "" && value != "unknown" && value != "0123456789abcdef"
}

func sameDevice(a, b Device) bool {
	if validIdentity(a.AndroidSerial) && validIdentity(b.AndroidSerial) {
		return strings.EqualFold(a.AndroidSerial, b.AndroidSerial)
	}
	if a.MAC != "" && b.MAC != "" && strings.EqualFold(a.MAC, b.MAC) {
		return true
	}
	if a.Hostname != "" && b.Hostname != "" && strings.EqualFold(a.Hostname, b.Hostname) {
		if a.Model == "" || b.Model == "" || strings.EqualFold(a.Model, b.Model) {
			return true
		}
	}
	if a.Model != "" && b.Model != "" && strings.EqualFold(a.Model, b.Model) &&
		a.Product != "" && b.Product != "" && strings.EqualFold(a.Product, b.Product) &&
		a.AndroidDevice != "" && b.AndroidDevice != "" && strings.EqualFold(a.AndroidDevice, b.AndroidDevice) {
		return true
	}
	return a.ID != "" && b.ID != "" && a.ID == b.ID
}

func (a *App) adb(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, a.adbPath, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timeout after %s", deadlineFromContext(ctx))
	}
	return output, err
}

func (a *App) adbDevice(ctx context.Context, serial string, args ...string) ([]byte, error) {
	withSerial := append([]string{"-s", serial}, args...)
	return a.adb(ctx, withSerial...)
}

func deadlineFromContext(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline)
}

func (a *App) adbDevices(ctx context.Context) ([]ADBDevice, error) {
	output, err := a.adb(ctx, "devices", "-l")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}

	var devices []ADBDevice
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devices = append(devices, ADBDevice{
			Serial: fields[0],
			State:  fields[1],
			Detail: strings.Join(fields[2:], " "),
		})
	}
	return devices, nil
}

func (a *App) adbConnect(ctx context.Context, ip string) error {
	if ip == "" {
		return errors.New("empty ip")
	}
	output, err := a.adb(ctx, "connect", net.JoinHostPort(ip, "5555"))
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	text := strings.ToLower(string(output))
	if strings.Contains(text, "unable") || strings.Contains(text, "failed") || strings.Contains(text, "refused") {
		return errors.New(strings.TrimSpace(string(output)))
	}
	return nil
}

func (a *App) adbDisconnect(ctx context.Context, serial string) {
	if serial == "" {
		return
	}
	_, _ = a.adb(ctx, "disconnect", serial)
}

func (a *App) adbState(ctx context.Context, serial string) string {
	devices, err := a.adbDevices(ctx)
	if err != nil {
		return ""
	}
	for _, dev := range devices {
		if dev.Serial == serial {
			return dev.State
		}
	}
	return ""
}

func adbDeviceFromListing(adbDev ADBDevice) Device {
	dev := Device{
		ADBSerial: adbDev.Serial,
		IP:        serialIP(adbDev.Serial),
		JarPort:   defaultJarPort,
		Status:    adbDev.State,
		ADBOK:     adbDev.State == "device",
		Source:    []string{"adb-devices"},
	}
	for _, field := range strings.Fields(adbDev.Detail) {
		key, value, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		value = strings.ReplaceAll(strings.TrimSpace(value), "_", " ")
		switch key {
		case "model":
			dev.Model = value
		case "product":
			dev.Product = value
		case "device":
			dev.AndroidDevice = value
		}
	}
	normalizeDevice(&dev)
	return dev
}

func serialIP(serial string) string {
	host, _, err := net.SplitHostPort(serial)
	if err == nil {
		return host
	}
	if ip := net.ParseIP(serial); ip != nil {
		return serial
	}
	return ""
}

func (a *App) readDeviceInfo(ctx context.Context, serial string) (Device, error) {
	script := strings.Join([]string{
		"printf '%s\\n' \"$(getprop ro.product.model 2>/dev/null)\"",
		"printf '%s\\n' \"$(getprop ro.product.name 2>/dev/null)\"",
		"printf '%s\\n' \"$(getprop ro.product.device 2>/dev/null)\"",
		"printf '%s\\n' \"$(getprop ro.serialno 2>/dev/null)\"",
		"printf '%s\\n' \"$(getprop ro.boot.serialno 2>/dev/null)\"",
		"printf '%s\\n' \"$(getprop net.hostname 2>/dev/null)\"",
		"printf '%s\\n' \"$(cat /sys/class/net/wlan0/address 2>/dev/null || cat /sys/class/net/eth0/address 2>/dev/null || cat /sys/class/net/en0/address 2>/dev/null)\"",
	}, "; ")
	output, err := a.adbDevice(ctx, serial, "shell", script)
	if err != nil {
		return Device{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}

	lines := splitLines(string(output))
	info := Device{
		ADBSerial:       serial,
		IP:              serialIP(serial),
		JarPort:         defaultJarPort,
		InjectMode:      envInt("JAR_INJECT_MODE", defaultInjectMode),
		DuplicateDropMS: envNonNegativeInt("JAR_DUPLICATE_DROP_MS", defaultDuplicateDropMS),
		ADBOK:           true,
		LastSeen:        time.Now(),
		Source:          []string{"adb"},
	}
	if len(lines) > 0 {
		info.Model = lines[0]
	}
	if len(lines) > 1 {
		info.Product = lines[1]
	}
	if len(lines) > 2 {
		info.AndroidDevice = lines[2]
	}
	if len(lines) > 3 {
		info.AndroidSerial = lines[3]
	}
	if !validIdentity(info.AndroidSerial) && len(lines) > 4 {
		info.AndroidSerial = lines[4]
	}
	if len(lines) > 5 {
		info.Hostname = lines[5]
	}
	if len(lines) > 6 {
		info.MAC = strings.ToUpper(lines[6])
	}
	normalizeDevice(&info)
	return info, nil
}

func splitLines(value string) []string {
	raw := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

func pingJar(ip string, port int, timeout time.Duration) error {
	if ip == "" {
		return errors.New("empty ip")
	}
	if port == 0 {
		port = defaultJarPort
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprint(conn, "ping\n"); err != nil {
		return err
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(response), "OK") {
		return fmt.Errorf("bad response: %s", strings.TrimSpace(response))
	}
	return nil
}

func sendUDPCommand(ip string, port int, command string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(command)); err != nil {
		return "", err
	}
	buffer := make([]byte, 2048)
	n, err := conn.Read(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}

func (a *App) deployPayload(ctx context.Context, serial, remotePath, pattern string, payload []byte) error {
	if output, err := a.adbDevice(ctx, serial, "shell", "mkdir -p /data/local/tmp"); err != nil {
		return fmt.Errorf("prepare remote tmp failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	output, err := a.adbDevice(ctx, serial, "push", tmpPath, remotePath)
	if err != nil {
		return fmt.Errorf("adb push failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (a *App) deployJar(ctx context.Context, serial string) error {
	return a.deployPayload(ctx, serial, keyServerDevicePath, "android-tv-agent-*.jar", keyServerJar)
}

func (a *App) deployNativeKeyServer(ctx context.Context, serial string) error {
	return a.deployPayload(ctx, serial, nativeKeyServerDevicePath, "android-tv-agent-linux-arm64-*", nativeKeyServer)
}

func (a *App) stopKeyServers(ctx context.Context, serial string) {
	remoteCommand := "for p in /proc/[0-9]*; do cmd=$(tr '\\0' ' ' < $p/cmdline 2>/dev/null); case \"$cmd\" in *AndroidTVAgent*|*KeyServer*|*android-tv-agent-linux-arm64*) kill ${p#/proc/} 2>/dev/null;; esac; done"
	_, _ = a.adbDevice(ctx, serial, "shell", remoteCommand)
}

func (a *App) startJar(ctx context.Context, dev Device) error {
	command := fmt.Sprintf(
		"setsid sh -c 'AP=app_process; for p in /system/bin/app_process /apex/com.android.runtime/bin/app_process64 /apex/com.android.runtime/bin/app_process32; do [ -x \"$p\" ] && AP=\"$p\" && break; done; CLASSPATH=%s exec \"$AP\" / AndroidTVAgent %d 0 %d %d >/data/local/tmp/android-tv-agent.log 2>&1 < /dev/null' >/dev/null 2>&1 &",
		keyServerDevicePath,
		dev.JarPort,
		dev.InjectMode,
		dev.DuplicateDropMS,
	)
	output, err := a.adbDevice(ctx, dev.ADBSerial, "shell", command)
	if err != nil {
		return fmt.Errorf("start jar failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (a *App) startNativeKeyServer(ctx context.Context, dev Device) error {
	command := fmt.Sprintf(
		"chmod 755 %[1]s; setsid sh -c '%[1]s -port %d -duplicate-drop-ms %d >/data/local/tmp/android-tv-agent.log 2>&1 < /dev/null' >/dev/null 2>&1 &",
		nativeKeyServerDevicePath,
		dev.JarPort,
		dev.DuplicateDropMS,
	)
	output, err := a.adbDevice(ctx, dev.ADBSerial, "shell", command)
	if err != nil {
		return fmt.Errorf("start native keyserver failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (a *App) detectKeyServerMode(ctx context.Context, serial string) (string, error) {
	script := strings.Join([]string{
		"[ -c /dev/uinput ] && echo linux-uinput && exit 0;",
		"[ -x /system/bin/app_process ] && echo android-jar && exit 0;",
		"[ -x /apex/com.android.runtime/bin/app_process64 ] && echo android-jar && exit 0;",
		"[ -x /apex/com.android.runtime/bin/app_process32 ] && echo android-jar && exit 0;",
		"command -v app_process >/dev/null 2>&1 && echo android-jar && exit 0;",
		"echo unsupported",
	}, " ")
	output, err := a.adbDevice(ctx, serial, "shell", script)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	mode := strings.TrimSpace(string(output))
	switch mode {
	case "android-jar", "linux-uinput":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported ADB environment: %s", mode)
	}
}

func (a *App) ensureJar(dev Device, reason string) Device {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	normalizeDevice(&dev)
	dev.LastChecked = time.Now()
	if dev.IP != "" {
		if err := pingJar(dev.IP, dev.JarPort, defaultJarTimeout); err == nil {
			dev.JarOK = true
			dev.Status = "running"
			dev.LastError = ""
			dev.LastSeen = time.Now()
			a.addLog("ok", "%s: keyserver already running on %s:%d", dev.Name, dev.IP, dev.JarPort)
			return dev
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultADBTimeout)
	dev = a.locateDevice(ctx, dev)
	adbState := ""
	if dev.ADBSerial != "" {
		adbState = a.adbState(ctx, dev.ADBSerial)
	}
	cancel()
	if dev.ADBSerial == "" || adbState != "device" {
		dev.ADBOK = false
		dev.JarOK = false
		dev.Status = "not_found"
		if dev.ADBSerial != "" && adbState != "" {
			dev.LastError = "ADB is " + adbState
			a.addLog("warn", "%s: ADB is %s on %s", dev.Name, adbState, dev.ADBSerial)
		} else {
			dev.LastError = "device not found by ADB scan"
			a.addLog("warn", "%s: not found by ADB", dev.Name)
		}
		return dev
	}

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mode, err := a.detectKeyServerMode(ctx, dev.ADBSerial)
	if err != nil {
		dev.Status = "unsupported_adb"
		dev.LastError = err.Error()
		a.addLog("error", "%s: %v", dev.Name, err)
		return dev
	}
	a.addLog("info", "%s: installing keyserver via %s mode=%s (%s)", dev.Name, dev.ADBSerial, mode, reason)
	a.stopKeyServers(ctx, dev.ADBSerial)
	var installErr error
	switch mode {
	case "android-jar":
		installErr = a.deployJar(ctx, dev.ADBSerial)
	case "linux-uinput":
		installErr = a.deployNativeKeyServer(ctx, dev.ADBSerial)
	}
	if installErr != nil {
		dev.Status = "install_failed"
		dev.LastError = installErr.Error()
		a.addLog("error", "%s: %v", dev.Name, installErr)
		return dev
	}
	var startErr error
	switch mode {
	case "android-jar":
		startErr = a.startJar(ctx, dev)
	case "linux-uinput":
		startErr = a.startNativeKeyServer(ctx, dev)
	}
	if startErr != nil {
		dev.Status = "start_failed"
		dev.LastError = startErr.Error()
		a.addLog("error", "%s: %v", dev.Name, startErr)
		return dev
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := pingJar(dev.IP, dev.JarPort, defaultJarTimeout); err == nil {
			dev.JarOK = true
			dev.ADBOK = true
			dev.Status = "running"
			dev.LastError = ""
			dev.LastSeen = time.Now()
			dev.LastInstalled = time.Now()
			a.addLog("ok", "%s: keyserver started on %s:%d", dev.Name, dev.IP, dev.JarPort)
			return dev
		}
		time.Sleep(400 * time.Millisecond)
	}

	dev.Status = "start_timeout"
	dev.LastError = "keyserver did not answer ping after start"
	a.addLog("error", "%s: keyserver did not answer after start", dev.Name)
	return dev
}

func (a *App) locateDevice(ctx context.Context, target Device) Device {
	normalizeDevice(&target)
	if target.ADBSerial != "" {
		ip := serialIP(target.ADBSerial)
		if ip == "" || tcpOpen(ip, 5555, time.Duration(envInt("SCAN_TIMEOUT_MS", defaultScanTimeoutMS))*time.Millisecond) {
			if a.adbState(ctx, target.ADBSerial) == "device" {
				if info, err := a.readDeviceInfo(ctx, target.ADBSerial); err == nil {
					return mergeDevice(target, info)
				}
			}
		} else {
			a.adbDisconnect(ctx, target.ADBSerial)
		}
	}

	devices, err := a.adbDevices(ctx)
	if err == nil {
		for _, adbDev := range devices {
			if adbDev.State != "device" {
				continue
			}
			ip := serialIP(adbDev.Serial)
			if ip != "" && !tcpOpen(ip, 5555, time.Duration(envInt("SCAN_TIMEOUT_MS", defaultScanTimeoutMS))*time.Millisecond) {
				a.adbDisconnect(ctx, adbDev.Serial)
				continue
			}
			listed := adbDeviceFromListing(adbDev)
			if sameDevice(target, listed) {
				if info, err := a.readDeviceInfo(ctx, adbDev.Serial); err == nil {
					return mergeDevice(target, mergeDevice(listed, info))
				}
				return mergeDevice(target, listed)
			}
			info, err := a.readDeviceInfo(ctx, adbDev.Serial)
			if err == nil && sameDevice(target, info) {
				return mergeDevice(target, info)
			}
		}
	}

	if target.IP != "" {
		_ = a.adbConnect(ctx, target.IP)
		serial := net.JoinHostPort(target.IP, "5555")
		if a.adbState(ctx, serial) == "device" {
			if info, err := a.readDeviceInfo(ctx, serial); err == nil {
				return mergeDevice(target, info)
			}
		}
	}

	results := a.scanNetwork(ctx)
	for _, found := range results {
		if sameDevice(target, *found) {
			return mergeDevice(target, *found)
		}
	}
	return target
}

func mergeDevice(old, newer Device) Device {
	remembered := old.Remembered
	lastInstalled := old.LastInstalled
	id := old.ID
	if id == "" {
		id = newer.ID
	}
	if newer.Name == "" || strings.HasPrefix(newer.Name, "192.") {
		newer.Name = old.Name
	}
	if newer.AndroidSerial == "" {
		newer.AndroidSerial = old.AndroidSerial
	}
	if newer.Model == "" {
		newer.Model = old.Model
	}
	if newer.Product == "" {
		newer.Product = old.Product
	}
	if newer.AndroidDevice == "" {
		newer.AndroidDevice = old.AndroidDevice
	}
	if newer.Hostname == "" {
		newer.Hostname = old.Hostname
	}
	if newer.MAC == "" {
		newer.MAC = old.MAC
	}
	if newer.JarPort == 0 {
		newer.JarPort = old.JarPort
	}
	if newer.InjectMode == 0 {
		newer.InjectMode = old.InjectMode
	}
	if newer.DuplicateDropMS == 0 {
		newer.DuplicateDropMS = old.DuplicateDropMS
	}
	newer.ID = id
	newer.Remembered = remembered
	newer.LastInstalled = lastInstalled
	normalizeDevice(&newer)
	return newer
}

func (a *App) rememberDevice(dev Device) Device {
	normalizeDevice(&dev)
	dev.Remembered = true

	a.mu.Lock()
	defer a.mu.Unlock()
	for id, existing := range a.devices {
		if sameDevice(*existing, dev) {
			dev.ID = id
			dev = mergeDevice(*existing, dev)
			dev.Remembered = true
			copy := dev
			a.devices[id] = &copy
			return copy
		}
	}
	copy := dev
	a.devices[copy.ID] = &copy
	return copy
}

func (a *App) updateDevice(dev Device) {
	normalizeDevice(&dev)
	a.mu.Lock()
	copy := dev
	a.devices[dev.ID] = &copy
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		a.addLog("error", "save state failed: %v", err)
	}
}

func (a *App) scanNetwork(ctx context.Context) []*Device {
	a.addLog("info", "scan started")
	found := make(map[string]*Device)
	timeout := time.Duration(envInt("SCAN_TIMEOUT_MS", defaultScanTimeoutMS)) * time.Millisecond

	devices, err := a.adbDevices(ctx)
	if err == nil {
		for _, adbDev := range devices {
			dev := adbDeviceFromListing(adbDev)
			if adbDev.State == "device" {
				if dev.IP != "" && !tcpOpen(dev.IP, 5555, timeout) {
					a.adbDisconnect(ctx, adbDev.Serial)
					continue
				}
				if info, err := a.readDeviceInfo(ctx, adbDev.Serial); err == nil {
					dev = mergeDevice(dev, info)
					dev.Source = appendSource(dev.Source, "adb")
				}
			}
			normalizeDevice(&dev)
			found[dev.ID] = &dev
		}
	} else {
		a.addLog("warn", "adb devices failed: %v", err)
	}

	hosts := scanHosts()
	type portResult struct {
		IP   string
		Port int
		Open bool
	}
	jobs := make(chan string)
	results := make(chan portResult)
	workers := 96
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				for _, port := range []int{5555, defaultJarPort} {
					results <- portResult{IP: ip, Port: port, Open: tcpOpen(ip, port, timeout)}
				}
			}
		}()
	}
	go func() {
		defer func() {
			close(jobs)
			wg.Wait()
			close(results)
		}()
		for _, ip := range hosts {
			select {
			case <-ctx.Done():
				return
			case jobs <- ip:
			}
		}
	}()

	openByIP := make(map[string]map[int]bool)
	for result := range results {
		if !result.Open {
			continue
		}
		if openByIP[result.IP] == nil {
			openByIP[result.IP] = make(map[int]bool)
		}
		openByIP[result.IP][result.Port] = true
	}

	for ip, ports := range openByIP {
		var dev Device
		if ports[5555] {
			_ = a.adbConnect(ctx, ip)
			serial := net.JoinHostPort(ip, "5555")
			if a.adbState(ctx, serial) == "device" {
				for _, adbDev := range mustADBDevices(a, ctx) {
					if adbDev.Serial == serial {
						dev = adbDeviceFromListing(adbDev)
						break
					}
				}
				info, err := a.readDeviceInfo(ctx, serial)
				if err == nil {
					dev = mergeDevice(dev, info)
				}
			}
		}
		if dev.IP == "" {
			dev = Device{IP: ip, JarPort: defaultJarPort}
		}
		if ports[defaultJarPort] {
			dev.JarOK = pingJar(ip, defaultJarPort, defaultJarTimeout) == nil
			dev.Source = appendSource(dev.Source, "jar")
		}
		if ports[5555] {
			dev.Source = appendSource(dev.Source, "adb:5555")
		}
		if dev.JarOK {
			dev.Status = "jar_running"
		} else if dev.ADBOK {
			dev.Status = "adb_online"
		} else {
			dev.Status = "found"
		}
		normalizeDevice(&dev)
		if existing, ok := found[dev.ID]; ok {
			merged := mergeDevice(*existing, dev)
			merged.Source = appendSource(existing.Source, dev.Source...)
			found[merged.ID] = &merged
		} else {
			found[dev.ID] = &dev
		}
	}

	list := make([]*Device, 0, len(found))
	for _, dev := range found {
		if remembered := a.matchRemembered(*dev); remembered != nil {
			merged := mergeDevice(*remembered, *dev)
			merged.Remembered = true
			*dev = merged
		}
		copy := *dev
		list = append(list, &copy)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})

	a.mu.Lock()
	a.scanResults = list
	a.mu.Unlock()
	a.addLog("ok", "scan finished: %d device(s)", len(list))
	return list
}

func (a *App) matchRemembered(dev Device) *Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, remembered := range a.devices {
		if sameDevice(*remembered, dev) {
			copy := *remembered
			return &copy
		}
	}
	return nil
}

func appendSource(sources []string, values ...string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, value := range append(sources, values...) {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func mustADBDevices(a *App, ctx context.Context) []ADBDevice {
	devices, err := a.adbDevices(ctx)
	if err != nil {
		return nil
	}
	return devices
}

func tcpOpen(ip string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func scanHosts() []string {
	if raw := strings.TrimSpace(os.Getenv("SCAN_CIDR")); raw != "" {
		return hostsFromCIDRs(strings.Split(raw, ","))
	}

	var cidrs []string
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !privateIPv4(ip) {
				continue
			}
			base := net.IPv4(ip[0], ip[1], ip[2], 0)
			cidrs = append(cidrs, base.String()+"/24")
		}
	}
	return hostsFromCIDRs(cidrs)
}

func privateIPv4(ip net.IP) bool {
	return ip[0] == 10 ||
		(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) ||
		(ip[0] == 192 && ip[1] == 168)
}

func hostsFromCIDRs(values []string) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		ip, ipNet, err := net.ParseCIDR(value)
		if err != nil {
			continue
		}
		ip = ip.To4()
		if ip == nil {
			continue
		}
		ones, bits := ipNet.Mask.Size()
		if bits != 32 {
			continue
		}
		if ones < 24 {
			ipNet.Mask = net.CIDRMask(24, 32)
			ipNet.IP = ip.Mask(ipNet.Mask)
		}
		for host := ipNet.IP.Mask(ipNet.Mask).To4(); ipNet.Contains(host); incIP(host) {
			if host[3] == 0 || host[3] == 255 {
				continue
			}
			s := net.IPv4(host[0], host[1], host[2], host[3]).String()
			if !seen[s] {
				seen[s] = true
				hosts = append(hosts, s)
			}
			if len(hosts) >= 1024 {
				return hosts
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func (a *App) watchdog() {
	if a.watchdogInterval <= 0 {
		return
	}
	a.addLog("info", "watchdog enabled: interval=%s", a.watchdogInterval)
	ticker := time.NewTicker(a.watchdogInterval)
	defer ticker.Stop()
	a.watchdogPass("startup")
	for range ticker.C {
		a.watchdogPass("tick")
	}
}

func (a *App) watchdogPass(reason string) {
	a.mu.Lock()
	devices := make([]Device, 0, len(a.devices))
	for _, dev := range a.devices {
		devices = append(devices, *dev)
	}
	a.mu.Unlock()

	for _, dev := range devices {
		updated := a.ensureJar(dev, reason)
		a.updateDevice(updated)
	}
}

func (a *App) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	devices := make([]*Device, 0, len(a.devices))
	for _, dev := range a.devices {
		copy := *dev
		devices = append(devices, &copy)
	}
	scan := make([]*Device, 0, len(a.scanResults))
	for _, dev := range a.scanResults {
		copy := *dev
		scan = append(scan, &copy)
	}
	logs := append([]LogEntry(nil), a.logs...)
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return map[string]any{
		"adb_path":          a.adbPath,
		"state_file":        a.statePath,
		"jar_port":          defaultJarPort,
		"watchdog_interval": a.watchdogInterval.String(),
		"devices":           devices,
		"scan":              scan,
		"logs":              logs,
	}
}

func (a *App) relayTarget() (Device, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var fallback *Device
	for _, dev := range a.devices {
		if dev.IP == "" {
			continue
		}
		if fallback == nil {
			copy := *dev
			fallback = &copy
		}
		if dev.JarOK || dev.Status == "running" {
			copy := *dev
			return copy, true
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Device{}, false
}

func normalizeRelayCommand(raw string) string {
	command := strings.TrimSpace(strings.Trim(raw, "\x00"))
	command = strings.TrimRight(command, "\r\n\t ")
	return command
}

func (a *App) relayCommand(command string) (Device, string, error) {
	command = normalizeRelayCommand(command)
	if command == "" {
		return Device{}, "", errors.New("empty command")
	}

	dev, ok := a.relayTarget()
	if !ok {
		return Device{}, "", errors.New("no remembered device")
	}

	response, err := sendUDPCommand(dev.IP, dev.JarPort, command, 3*time.Second)
	if err == nil {
		a.addLog("ok", "%s: relay command OK: %s", dev.Name, strings.TrimSpace(command))
		return dev, response, nil
	}

	a.addLog("warn", "%s: relay failed, trying watchdog refresh: %v", dev.Name, err)
	updated := a.ensureJar(dev, "relay")
	a.updateDevice(updated)
	response, err = sendUDPCommand(updated.IP, updated.JarPort, command, 3*time.Second)
	if err != nil {
		a.addLog("error", "%s: relay command failed: %v", updated.Name, err)
		return updated, "", err
	}
	a.addLog("ok", "%s: relay command OK: %s", updated.Name, strings.TrimSpace(command))
	return updated, response, nil
}

func (a *App) startUDPRelay(port string) {
	conn, err := net.ListenPacket("udp", ":"+port)
	if err != nil {
		a.addLog("error", "udp relay listen on :%s failed: %v", port, err)
		return
	}
	a.addLog("info", "udp relay listening on :%s", port)

	buffer := make([]byte, 4096)
	for {
		n, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			a.addLog("error", "udp relay read failed: %v", err)
			continue
		}
		packet := append([]byte(nil), buffer[:n]...)
		go func(addr net.Addr, packet []byte) {
			command := string(packet)
			a.addLog("info", "udp relay rx remote=%s bytes=%d command=%q", addr.String(), len(packet), normalizeRelayCommand(command))
			_, response, err := a.relayCommand(command)
			if err != nil {
				response = "ERR " + err.Error()
			}
			if !strings.HasSuffix(response, "\n") {
				response += "\n"
			}
			if _, err := conn.WriteTo([]byte(response), addr); err != nil {
				a.addLog("error", "udp relay reply failed to %s: %v", addr.String(), err)
			}
		}(addr, packet)
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/scan", a.handleScan)
	mux.HandleFunc("/api/devices", a.handleRemember)
	mux.HandleFunc("/api/devices/", a.handleDeviceAction)
	return mux
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTemplate.Execute(w, nil)
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.snapshot())
}

func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	a.scanNetwork(ctx)
	writeJSON(w, a.snapshot())
}

func (a *App) handleRemember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var dev Device
	if err := json.NewDecoder(r.Body).Decode(&dev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dev = a.rememberDevice(dev)
	if err := a.saveState(); err != nil {
		a.addLog("error", "save state failed: %v", err)
	}
	a.addLog("ok", "remembered %s (%s)", dev.Name, dev.IP)
	writeJSON(w, dev)
}

func (a *App) handleDeviceAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	a.mu.Lock()
	dev, ok := a.devices[id]
	var copy Device
	if ok {
		copy = *dev
	}
	a.mu.Unlock()
	if !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	switch action {
	case "install":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		updated := a.ensureJar(copy, "manual")
		a.updateDevice(updated)
		writeJSON(w, updated)
	case "forget":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "POST/DELETE required", http.StatusMethodNotAllowed)
			return
		}
		a.mu.Lock()
		delete(a.devices, id)
		a.mu.Unlock()
		_ = a.saveState()
		a.addLog("ok", "forgot %s", copy.Name)
		writeJSON(w, map[string]bool{"ok": true})
	case "test":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Command string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.Command) == "" {
			body.Command = "input keyevent 22"
		}
		response, err := sendUDPCommand(copy.IP, copy.JarPort, body.Command, 3*time.Second)
		if err != nil {
			a.addLog("error", "%s: test command failed: %v", copy.Name, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		a.addLog("ok", "%s: test command OK: %s", copy.Name, strings.TrimSpace(body.Command))
		writeJSON(w, map[string]string{"response": response})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

var pageTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Android TV Go Proxy</title>
  <style>
    :root { color-scheme: light; font-family: Inter, Segoe UI, Arial, sans-serif; }
    body { margin: 0; background: #f4f6f8; color: #17202a; }
    header { background: #102033; color: white; padding: 18px 24px; }
    header h1 { margin: 0; font-size: 22px; font-weight: 650; }
    main { max-width: 1180px; margin: 0 auto; padding: 20px; }
    .toolbar { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; margin-bottom: 16px; }
    button { border: 0; border-radius: 6px; padding: 9px 12px; background: #1d72b8; color: white; cursor: pointer; font-weight: 600; }
    button.secondary { background: #607080; }
    button.danger { background: #ad3434; }
    button:disabled { opacity: .55; cursor: wait; }
    input { border: 1px solid #c7d0d9; border-radius: 6px; padding: 9px 10px; min-width: 240px; }
    section { background: white; border: 1px solid #dce3ea; border-radius: 8px; margin: 16px 0; overflow: hidden; }
    section h2 { margin: 0; padding: 14px 16px; font-size: 16px; border-bottom: 1px solid #e5ebf0; background: #fbfcfd; }
    table { width: 100%; border-collapse: collapse; font-size: 14px; }
    th, td { padding: 10px 12px; border-bottom: 1px solid #eef2f5; text-align: left; vertical-align: top; }
    th { color: #526170; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
    .muted { color: #687786; }
    .pill { display: inline-block; border-radius: 999px; padding: 3px 8px; background: #edf2f7; font-size: 12px; margin: 1px; }
    .ok { background: #dcf7e8; color: #11653a; }
    .bad { background: #fde6e6; color: #8f2020; }
    .warn { background: #fff2cc; color: #7a5400; }
    pre { margin: 0; padding: 12px 16px; max-height: 260px; overflow: auto; background: #0b1220; color: #d7e3f4; font-size: 12px; }
    .row-actions { display: flex; flex-wrap: wrap; gap: 6px; }
    @media (max-width: 800px) {
      table, thead, tbody, tr, td, th { display: block; }
      thead { display: none; }
      tr { border-bottom: 1px solid #dce3ea; }
      td { border: 0; padding: 8px 12px; }
      td::before { content: attr(data-label); display: block; color: #687786; font-size: 12px; margin-bottom: 2px; }
    }
  </style>
</head>
<body>
  <header><h1>Android TV Go Proxy</h1></header>
  <main>
    <div class="toolbar">
      <button id="scanBtn" onclick="scan()">Сканировать сеть</button>
      <button class="secondary" onclick="refresh()">Обновить</button>
      <input id="testCommand" value="input keyevent 22" aria-label="test command">
      <span class="muted" id="meta"></span>
    </div>

    <section>
      <h2>Запомненные приставки</h2>
      <table>
        <thead><tr><th>Устройство</th><th>Адрес</th><th>Статус</th><th>Примеры команд</th><th>Действия</th></tr></thead>
        <tbody id="devices"></tbody>
      </table>
    </section>

    <section>
      <h2>Найдено в сети</h2>
      <table>
        <thead><tr><th>Устройство</th><th>Адрес</th><th>Источник</th><th>Статус</th><th>Действия</th></tr></thead>
        <tbody id="scan"></tbody>
      </table>
    </section>

    <section>
      <h2>Логи</h2>
      <pre id="logs"></pre>
    </section>
  </main>
<script>
let state = {};
const esc = s => String(s ?? '').replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
function pill(text, cls='') { return '<span class="pill '+cls+'">'+esc(text)+'</span>'; }
function addr(d) {
  const port = d.jar_port || 17891;
  return '<div>'+esc(d.ip || '-')+'</div><div class="muted">ADB: '+esc(d.adb_serial || '-')+'</div><div class="muted">UDP/TCP: '+esc(d.ip || '-')+':'+port+'</div>';
}
function status(d) {
  let out = '';
  out += pill(d.jar_ok ? 'jar ok' : 'jar ?', d.jar_ok ? 'ok' : 'warn');
  out += pill(d.adb_ok ? 'adb ok' : 'adb ?', d.adb_ok ? 'ok' : 'warn');
  if (d.status) out += pill(d.status);
  if (d.last_error) out += '<div class="muted">'+esc(d.last_error)+'</div>';
  return out;
}
function deviceTitle(d) {
  return '<strong>'+esc(d.name || d.hostname || d.model || d.ip || 'Android TV')+'</strong>'
    + '<div class="muted">'+esc(d.model || '')+' '+esc(d.android_device || '')+'</div>'
    + '<div class="muted">'+esc(d.hostname || '')+' '+esc(d.mac || '')+'</div>';
}
function commandExamples(d) {
  const ip = d.ip || 'IP';
  const port = d.jar_port || 17891;
  return '<div><strong>UDP '+esc(ip)+':'+port+'</strong></div>'
    + '<div class="muted">\'input keyevent 21\',13</div>'
    + '<div class="muted">\'cmd=shell monkey -p ru.kinopoisk.tv 1\',13</div>';
}
async function api(url, opts={}) {
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(await res.text());
  return await res.json();
}
async function refresh() {
  state = await api('/api/state');
  render();
}
async function scan() {
  const btn = document.getElementById('scanBtn');
  btn.disabled = true;
  try { state = await api('/api/scan', {method:'POST'}); render(); }
  catch (e) { alert(e.message); }
  finally { btn.disabled = false; }
}
async function remember(idx) {
  const d = state.scan[idx];
  await api('/api/devices', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(d)});
  await refresh();
}
async function install(id) {
  await api('/api/devices/'+encodeURIComponent(id)+'/install', {method:'POST'});
  await refresh();
}
async function forget(id) {
  await api('/api/devices/'+encodeURIComponent(id)+'/forget', {method:'POST'});
  await refresh();
}
async function test(id) {
  const command = document.getElementById('testCommand').value;
  try {
    await api('/api/devices/'+encodeURIComponent(id)+'/test', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({command})});
  } catch(e) { console.error(e); }
  await refresh();
}
function renderDevices() {
  const rows = (state.devices || []).map(d =>
    '<tr>'
    + '<td data-label="Устройство">'+deviceTitle(d)+'</td>'
    + '<td data-label="Адрес">'+addr(d)+'</td>'
    + '<td data-label="Статус">'+status(d)+'</td>'
    + '<td data-label="Команды">'+commandExamples(d)+'</td>'
    + '<td data-label="Действия"><div class="row-actions">'
    + '<button data-action="install" data-id="'+esc(d.id)+'">Install/Restart</button>'
    + '<button class="secondary" data-action="test" data-id="'+esc(d.id)+'">Test</button>'
    + '<button class="danger" data-action="forget" data-id="'+esc(d.id)+'">Forget</button>'
    + '</div></td>'
    + '</tr>').join('');
  document.getElementById('devices').innerHTML = rows || '<tr><td colspan="5" class="muted">Пока ничего не запомнено. Нажми сканирование и выбери приставку.</td></tr>';
}
function renderScan() {
  const rows = (state.scan || []).map((d,i) =>
    '<tr>'
    + '<td data-label="Устройство">'+deviceTitle(d)+'</td>'
    + '<td data-label="Адрес">'+addr(d)+'</td>'
    + '<td data-label="Источник">'+(d.source || []).map(s => pill(s)).join('')+'</td>'
    + '<td data-label="Статус">'+status(d)+'</td>'
    + '<td data-label="Действия"><button data-action="remember" data-index="'+i+'">Запомнить</button></td>'
    + '</tr>').join('');
  document.getElementById('scan').innerHTML = rows || '<tr><td colspan="5" class="muted">Нажми "Сканировать сеть".</td></tr>';
}
function renderLogs() {
  const lines = (state.logs || []).slice(-120).map(l => new Date(l.time).toLocaleTimeString()+' ['+l.level+'] '+l.text);
  document.getElementById('logs').textContent = lines.join('\n');
}
function render() {
  document.getElementById('meta').textContent = 'ADB: '+(state.adb_path || '-')+' | watchdog: '+(state.watchdog_interval || '-')+' | state: '+(state.state_file || '-');
  renderDevices();
  renderScan();
  renderLogs();
}
document.addEventListener('click', function(e) {
  const btn = e.target.closest('button[data-action]');
  if (!btn) return;
  const action = btn.getAttribute('data-action');
  if (action === 'install') install(btn.getAttribute('data-id'));
  if (action === 'test') test(btn.getAttribute('data-id'));
  if (action === 'forget') forget(btn.getAttribute('data-id'));
  if (action === 'remember') remember(Number(btn.getAttribute('data-index')));
});
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`))

func main() {
	app := newApp()
	if err := app.loadState(); err != nil {
		log.Printf("[error] load state failed: %v", err)
	}

	if seed := strings.TrimSpace(os.Getenv("ADB_SERIAL")); seed != "" {
		ctx, cancel := context.WithTimeout(context.Background(), defaultADBTimeout)
		info, err := app.readDeviceInfo(ctx, seed)
		cancel()
		if err == nil {
			info.Remembered = true
			info.Status = "seeded"
			app.updateDevice(info)
			app.addLog("ok", "seeded device from ADB_SERIAL: %s", seed)
		} else {
			app.addLog("warn", "ADB_SERIAL seed failed: %v", err)
		}
	}

	go app.watchdog()

	port := getenv("WEB_PORT", defaultWebPort)
	go app.startUDPRelay(port)
	app.addLog("info", "web UI listening on :%s", port)
	if err := http.ListenAndServe(":"+port, app.routes()); err != nil {
		log.Fatal(err)
	}
}
