package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultPort            = 17891
	defaultDuplicateDropMS = 90
	readIdleTimeout        = 35 * time.Millisecond
	shellTimeout           = 10 * time.Second

	evSyn = 0x00
	evKey = 0x01

	synReport = 0

	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiSetEvbit   = 0x40045564
	uiSetKeybit  = 0x40045565
)

type commandResult struct {
	response string
	log      string
}

type responder func(string) error

type keyServer struct {
	uinput           *os.File
	duplicateDropMS  int
	lastCommandAtMS  int64
	lastExecuted     string
	lastExecutedAtMS int64
	mu               sync.Mutex
}

type inputID struct {
	Bus     uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type uinputUserDev struct {
	Name         [80]byte
	ID           inputID
	FFEffectsMax int32
	AbsMax       [64]int32
	AbsMin       [64]int32
	AbsFuzz      [64]int32
	AbsFlat      [64]int32
}

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

func main() {
	port := flag.Int("port", defaultPort, "TCP/UDP listen port")
	duplicateDropMS := flag.Int("duplicate-drop-ms", defaultDuplicateDropMS, "duplicate keyevent drop window")
	flag.Parse()
	if flag.NArg() > 0 {
		if parsed, err := strconv.Atoi(flag.Arg(0)); err == nil && parsed > 0 {
			*port = parsed
		}
	}
	if flag.NArg() > 3 {
		if parsed, err := strconv.Atoi(flag.Arg(3)); err == nil && parsed >= 0 {
			*duplicateDropMS = parsed
		}
	}

	server, err := newKeyServer(*duplicateDropMS)
	if err != nil {
		log.Fatalf("init uinput: %v", err)
	}
	defer server.close()

	if err := server.serve(*port); err != nil {
		log.Fatal(err)
	}
}

func newKeyServer(duplicateDropMS int) (*keyServer, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	if err := ioctl(f, uiSetEvbit, evKey); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("UI_SET_EVBIT EV_KEY: %w", err)
	}
	if err := ioctl(f, uiSetEvbit, evSyn); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("UI_SET_EVBIT EV_SYN: %w", err)
	}
	for _, code := range supportedLinuxKeys() {
		if err := ioctl(f, uiSetKeybit, code); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("UI_SET_KEYBIT %d: %w", code, err)
		}
	}

	var dev uinputUserDev
	copy(dev.Name[:], []byte("AndroidTVAgent"))
	dev.ID = inputID{Bus: 0x03, Vendor: 0x1209, Product: 0x1789, Version: 1}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, dev); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write uinput_user_dev: %w", err)
	}
	if err := ioctl(f, uiDevCreate, 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	time.Sleep(150 * time.Millisecond)

	return &keyServer{uinput: f, duplicateDropMS: duplicateDropMS}, nil
}

func (s *keyServer) close() {
	if s.uinput == nil {
		return
	}
	_ = ioctl(s.uinput, uiDevDestroy, 0)
	_ = s.uinput.Close()
}

func ioctl(f *os.File, request uintptr, value int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func (s *keyServer) serve(port int) error {
	if err := s.startUDP(port); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	log.Printf("READY TCP/UDP %d native=uinput duplicateDropMS=%d", port, s.duplicateDropMS)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go s.handleTCP(conn)
	}
}

func (s *keyServer) startUDP(port int) error {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		defer conn.Close()
		log.Printf("UDP ready %d", port)
		buf := make([]byte, 2048)
		for {
			n, client, err := conn.ReadFromUDP(buf)
			if err != nil {
				log.Printf("UDP ERR %v", err)
				return
			}
			command := string(buf[:n])
			s.drain(command, true, func(response string) error {
				_, err := conn.WriteToUDP([]byte(response+"\n"), client)
				return err
			})
		}
	}()
	return nil
}

func (s *keyServer) handleTCP(conn net.Conn) {
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetNoDelay(true)
	reader := bufio.NewReader(conn)
	var b strings.Builder
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readIdleTimeout))
		part, err := reader.ReadString('\n')
		if len(part) > 0 {
			b.WriteString(part)
			s.drainBuilder(&b, false, func(response string) error {
				_, err := io.WriteString(conn, response+"\n")
				return err
			})
		}
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			s.drainBuilder(&b, true, func(response string) error {
				_, err := io.WriteString(conn, response+"\n")
				return err
			})
			continue
		}
		s.drainBuilder(&b, true, func(response string) error {
			_, err := io.WriteString(conn, response+"\n")
			return err
		})
		return
	}
}

func (s *keyServer) drain(value string, flushPartial bool, reply responder) {
	var b strings.Builder
	b.WriteString(value)
	s.drainBuilder(&b, flushPartial, reply)
}

func (s *keyServer) drainBuilder(b *strings.Builder, flushPartial bool, reply responder) {
	value := strings.ReplaceAll(b.String(), "\r", "\n")
	b.Reset()

	for {
		idx := strings.IndexByte(value, '\n')
		if idx < 0 {
			break
		}
		s.dispatch(value[:idx], reply)
		value = value[idx+1:]
	}
	if flushPartial && strings.TrimSpace(value) != "" {
		s.dispatch(value, reply)
		return
	}
	b.WriteString(value)
}

func (s *keyServer) dispatch(line string, reply responder) {
	command, respond := normalizeCommand(line)
	if command == "" {
		return
	}
	nowWall := time.Now().UnixMilli()
	gap := s.commandGap(nowWall)
	log.Printf("RX gap=%dms reply=%v cmd=%s", gap, respond, safeLog(command))

	canonical := canonicalKeyCommand(command)
	now := time.Now().UnixMilli()
	if s.shouldDropDuplicate(canonical, now) {
		log.Printf("DROP duplicate threshold=%dms cmd=%s", s.duplicateDropMS, safeLog(command))
		if respond && reply != nil {
			_ = reply("OK dropped duplicate")
		}
		return
	}

	started := time.Now()
	result, err := s.execute(command)
	if err != nil {
		log.Printf("ERR dur=%s cmd=%s err=%s", time.Since(started), safeLog(command), safeLog(err.Error()))
		if respond && reply != nil {
			_ = reply("ERR " + err.Error())
		}
		return
	}
	s.markExecuted(canonical, now)
	log.Printf("OK dur=%s cmd=%s%s", time.Since(started), safeLog(command), result.log)
	if respond && reply != nil {
		_ = reply(result.response)
	}
}

func normalizeCommand(line string) (string, bool) {
	command := strings.TrimSpace(line)
	if strings.HasPrefix(command, "noreply ") {
		return strings.TrimSpace(strings.TrimPrefix(command, "noreply ")), false
	}
	if strings.HasPrefix(command, "cmd=") || strings.HasPrefix(command, "type=") {
		values, err := url.ParseQuery(command)
		if err == nil {
			if cmd := strings.TrimSpace(values.Get("cmd")); cmd != "" {
				return cmd, true
			}
		}
	}
	return command, true
}

func canonicalKeyCommand(command string) string {
	normalized := stripShellPrefix(strings.TrimSpace(command))
	if strings.HasPrefix(normalized, "input ") {
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "input "))
	}
	if strings.HasPrefix(normalized, "keyevent ") {
		return normalized
	}
	return ""
}

func stripShellPrefix(command string) string {
	command = strings.TrimSpace(command)
	if strings.HasPrefix(command, "adb shell ") {
		return strings.TrimSpace(strings.TrimPrefix(command, "adb shell "))
	}
	if strings.HasPrefix(command, "shell ") {
		return strings.TrimSpace(strings.TrimPrefix(command, "shell "))
	}
	return command
}

func (s *keyServer) commandGap(now int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCommandAtMS == 0 {
		s.lastCommandAtMS = now
		return -1
	}
	gap := now - s.lastCommandAtMS
	s.lastCommandAtMS = now
	return gap
}

func (s *keyServer) shouldDropDuplicate(canonical string, now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicateDropMS > 0 && canonical != "" && canonical == s.lastExecuted && now-s.lastExecutedAtMS < int64(s.duplicateDropMS)
}

func (s *keyServer) markExecuted(canonical string, now int64) {
	if canonical == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastExecuted = canonical
	s.lastExecutedAtMS = now
}

func (s *keyServer) execute(command string) (commandResult, error) {
	if command == "ping" {
		return commandResult{response: "OK pong"}, nil
	}

	normalized := stripShellPrefix(command)
	if strings.HasPrefix(normalized, "input ") {
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "input "))
	}
	parts := strings.Fields(normalized)
	if len(parts) >= 2 && parts[0] == "keyevent" {
		longPress := false
		var keys []int
		for _, part := range parts[1:] {
			if part == "--longpress" {
				longPress = true
				continue
			}
			key, err := resolveLinuxKey(part)
			if err != nil {
				return commandResult{}, err
			}
			keys = append(keys, key)
		}
		if len(keys) == 0 {
			return commandResult{}, errors.New("missing keycode")
		}
		for _, key := range keys {
			if err := s.injectKey(key, longPress); err != nil {
				return commandResult{}, err
			}
		}
		return commandResult{response: "OK"}, nil
	}

	shellCommand := stripShellPrefix(command)
	if shellCommand == command {
		return commandResult{}, errors.New("unsupported command")
	}
	return runShell(shellCommand)
}

func (s *keyServer) injectKey(code int, longPress bool) error {
	if err := s.writeEvent(evKey, code, 1); err != nil {
		return err
	}
	if err := s.sync(); err != nil {
		return err
	}
	if longPress {
		time.Sleep(500 * time.Millisecond)
		if err := s.writeEvent(evKey, code, 2); err != nil {
			return err
		}
		if err := s.sync(); err != nil {
			return err
		}
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.writeEvent(evKey, code, 0); err != nil {
		return err
	}
	return s.sync()
}

func (s *keyServer) sync() error {
	return s.writeEvent(evSyn, synReport, 0)
}

func (s *keyServer) writeEvent(eventType, code int, value int32) error {
	now := time.Now()
	event := inputEvent{
		Sec:   now.Unix(),
		Usec:  int64(now.Nanosecond() / 1000),
		Type:  uint16(eventType),
		Code:  uint16(code),
		Value: value,
	}
	return binary.Write(s.uinput, binary.LittleEndian, event)
}

func runShell(shellCommand string) (commandResult, error) {
	ctx := exec.Command("sh", "-c", shellCommand)
	var stdout, stderr bytes.Buffer
	ctx.Stdout = &stdout
	ctx.Stderr = &stderr
	done := make(chan error, 1)
	if err := ctx.Start(); err != nil {
		return commandResult{}, err
	}
	go func() { done <- ctx.Wait() }()

	select {
	case err := <-done:
		out := strings.TrimSpace(stdout.String())
		errText := strings.TrimSpace(stderr.String())
		if err != nil {
			if errText == "" {
				errText = out
			}
			return commandResult{}, fmt.Errorf("shell %v %s", err, trimForResponse(errText))
		}
		response := "OK"
		if out != "" {
			response += "\n" + trimForResponse(out)
		}
		return commandResult{response: response, log: fmt.Sprintf(" shell=%s output=%d err=%d", safeLog(shellCommand), stdout.Len(), stderr.Len())}, nil
	case <-time.After(shellTimeout):
		_ = ctx.Process.Kill()
		return commandResult{}, errors.New("shell timeout")
	}
}

func trimForResponse(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1024 {
		return value[:1024] + "..."
	}
	return value
}

func safeLog(value string) string {
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}

func supportedLinuxKeys() []int {
	keys := map[int]bool{}
	for _, key := range androidToLinuxKey {
		keys[key] = true
	}
	for i := 1; i <= 255; i++ {
		keys[i] = true
	}
	keys[352] = true
	out := make([]int, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	return out
}

func resolveLinuxKey(value string) (int, error) {
	if parsed, err := strconv.Atoi(value); err == nil {
		if mapped, ok := androidToLinuxKey[parsed]; ok {
			return mapped, nil
		}
		return parsed, nil
	}
	name := strings.ToUpper(value)
	name = strings.TrimPrefix(name, "KEYCODE_")
	name = strings.TrimPrefix(name, "KEY_")
	if code, ok := androidNameToLinuxKey[name]; ok {
		return code, nil
	}
	return 0, fmt.Errorf("unknown keycode %q", value)
}

var androidToLinuxKey = map[int]int{
	3:   102, // HOME
	4:   158, // BACK
	7:   11,  // 0
	8:   2,   // 1
	9:   3,   // 2
	10:  4,   // 3
	11:  5,   // 4
	12:  6,   // 5
	13:  7,   // 6
	14:  8,   // 7
	15:  9,   // 8
	16:  10,  // 9
	19:  103, // DPAD_UP
	20:  108, // DPAD_DOWN
	21:  105, // DPAD_LEFT
	22:  106, // DPAD_RIGHT
	23:  28,  // DPAD_CENTER
	24:  115, // VOLUME_UP
	25:  114, // VOLUME_DOWN
	26:  116, // POWER
	62:  57,  // SPACE
	66:  28,  // ENTER
	67:  14,  // DEL
	82:  139, // MENU
	85:  164, // MEDIA_PLAY_PAUSE
	86:  166, // MEDIA_STOP
	87:  163, // MEDIA_NEXT
	88:  165, // MEDIA_PREVIOUS
	89:  168, // MEDIA_REWIND
	90:  208, // MEDIA_FAST_FORWARD
	111: 1,   // ESCAPE
	164: 113, // VOLUME_MUTE
}

var androidNameToLinuxKey = map[string]int{
	"HOME":               102,
	"BACK":               158,
	"DPAD_UP":            103,
	"UP":                 103,
	"DPAD_DOWN":          108,
	"DOWN":               108,
	"DPAD_LEFT":          105,
	"LEFT":               105,
	"DPAD_RIGHT":         106,
	"RIGHT":              106,
	"DPAD_CENTER":        28,
	"CENTER":             28,
	"ENTER":              28,
	"OK":                 28,
	"MENU":               139,
	"SPACE":              57,
	"DEL":                14,
	"DELETE":             14,
	"ESCAPE":             1,
	"VOLUME_UP":          115,
	"VOLUME_DOWN":        114,
	"VOLUME_MUTE":        113,
	"MUTE":               113,
	"POWER":              116,
	"MEDIA_PLAY_PAUSE":   164,
	"PLAY_PAUSE":         164,
	"MEDIA_STOP":         166,
	"STOP":               166,
	"MEDIA_NEXT":         163,
	"NEXT":               163,
	"MEDIA_PREVIOUS":     165,
	"PREVIOUS":           165,
	"MEDIA_REWIND":       168,
	"REWIND":             168,
	"MEDIA_FAST_FORWARD": 208,
	"FAST_FORWARD":       208,
	"0":                  11,
	"1":                  2,
	"2":                  3,
	"3":                  4,
	"4":                  5,
	"5":                  6,
	"6":                  7,
	"7":                  8,
	"8":                  9,
	"9":                  10,
}
