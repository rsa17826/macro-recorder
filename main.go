// Command macro is a standalone daemon that lets you record keyboard macros
// and bind them to keys, on top of the existing IMan / go-input-lib stack
// used in main.go.
//
// Controls (default):
//
//	RightAlt + <key>              -> toggle recording for the macro slot "<key>"
//	                                   (press once to start, press the SAME key
//	                                   again while RightAlt is held to stop and save)
//	RightAlt + Shift + <key>      -> play back the macro saved to slot "<key>"
//	LeftCtrl + RightAlt + <key>   -> open the macro's text file in your editor
//	Esc                            -> abort whatever macro is currently playing
//
// Each macro is stored as its own plain text file under macros/<key>.macro:
//
//	delay=true
//
//	{a}{delay 3048ms}{t down}
//
// The first line is "delay=true" or "delay=false" (whether {delay ...}
// blocks are honored during playback). A blank line follows, then the
// macro body itself (you can spread it over multiple lines for
// readability -- newlines are stripped before playback).
//
// Block syntax understood during playback:
//
//	{a}            tap 'a' once (down then up)
//	{a down}       press 'a' and hold
//	{a up}         release 'a'
//	{f3 4}         tap 'f3' four times in a row
//	{delay 300ms}  sleep 300ms (skipped entirely if delay=false)
//
// "^" and "+" shorthand (ctrl/shift) and bare literal characters from
// main.go's original ExecuteAHKSequence are still supported.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	input "github.com/rsa17826/go-input-lib"
	pp "github.com/rsa17826/gopp"
	"github.com/rsa17826/input-manager/IMan"
)

// ─────────────────────────────────────────────────────────────────────────
// Key name <-> code table
// ─────────────────────────────────────────────────────────────────────────

// namedKeys covers every non-printable / control key in types.go.
var namedKeys = map[string]uint16{
	"esc": input.KEY_ESC, "tab": input.KEY_TAB, "enter": input.KEY_ENTER,
	"backspace": input.KEY_BACKSPACE, "space": input.KEY_SPACE,
	"capslock": input.KEY_CAPSLOCK, "numlock": input.KEY_NUMLOCK, "scrolllock": input.KEY_SCROLLLOCK,
	"ctrl": input.KEY_LEFTCTRL, "lctrl": input.KEY_LEFTCTRL, "rctrl": input.KEY_RIGHTCTRL,
	"shift": input.KEY_LEFTSHIFT, "lshift": input.KEY_LEFTSHIFT, "rshift": input.KEY_RIGHTSHIFT,
	"alt": input.KEY_LEFTALT, "lalt": input.KEY_LEFTALT, "ralt": input.KEY_RIGHTALT,
	"meta": input.KEY_LEFTMETA, "lmeta": input.KEY_LEFTMETA, "rmeta": input.KEY_RIGHTMETA, "win": input.KEY_LEFTMETA,
	"home": input.KEY_HOME, "end": input.KEY_END, "insert": input.KEY_INSERT,
	"del": input.KEY_DELETE, "delete": input.KEY_DELETE,
	"pageup": input.KEY_PAGEUP, "pagedown": input.KEY_PAGEDOWN,
	"up": input.KEY_UP, "down": input.KEY_DOWN, "left": input.KEY_LEFT, "right": input.KEY_RIGHT,
	"pause": input.KEY_PAUSE, "sysrq": input.KEY_SYSRQ, "printscreen": input.KEY_SYSRQ,
	"kp0": input.KEY_KP0, "kp1": input.KEY_KP1, "kp2": input.KEY_KP2, "kp3": input.KEY_KP3, "kp4": input.KEY_KP4,
	"kp5": input.KEY_KP5, "kp6": input.KEY_KP6, "kp7": input.KEY_KP7, "kp8": input.KEY_KP8, "kp9": input.KEY_KP9,
	"kpdot": input.KEY_KPDOT, "kpslash": input.KEY_KPSLASH, "kpasterisk": input.KEY_KPASTERISK,
	"kpminus": input.KEY_KPMINUS, "kpplus": input.KEY_KPPLUS, "kpenter": input.KEY_KPENTER, "kpequal": input.KEY_KPEQUAL,
	"f1": input.KEY_F1, "f2": input.KEY_F2, "f3": input.KEY_F3, "f4": input.KEY_F4, "f5": input.KEY_F5,
	"f6": input.KEY_F6, "f7": input.KEY_F7, "f8": input.KEY_F8, "f9": input.KEY_F9, "f10": input.KEY_F10,
	"f11": input.KEY_F11, "f12": input.KEY_F12, "f13": input.KEY_F13, "f14": input.KEY_F14, "f15": input.KEY_F15,
	"f16": input.KEY_F16, "f17": input.KEY_F17, "f18": input.KEY_F18, "f19": input.KEY_F19, "f20": input.KEY_F20,
	"f21": input.KEY_F21, "f22": input.KEY_F22, "f23": input.KEY_F23, "f24": input.KEY_F24,
}

// codeToName is built at init time: printable keys map to their unshifted
// character, everything else falls back to namedKeys, and anything totally
// unknown falls back to "codeN".
var codeToName = map[uint16]string{}

func init() {
	for r, info := range input.CharKeyMap {
		if info.Shift {
			continue // only keep the unshifted rune as the canonical name
		}
		if _, exists := codeToName[info.Code]; !exists {
			codeToName[info.Code] = string(r)
		}
	}
	for name, code := range namedKeys {
		if _, exists := codeToName[code]; !exists {
			codeToName[code] = name
		}
	}
}

// KeyName returns the canonical macro-block / filename-safe name for a keycode.
func KeyName(code uint16) string {
	if name, ok := codeToName[code]; ok {
		return name
	}
	return fmt.Sprintf("code%d", code)
}

// KeyCodeByName resolves a macro-block name back to a keycode.
func KeyCodeByName(name string) (uint16, bool) {
	name = strings.ToLower(name)
	if code, ok := namedKeys[name]; ok {
		return code, true
	}
	if strings.HasPrefix(name, "code") {
		if n, err := strconv.Atoi(name[4:]); err == nil {
			return uint16(n), true
		}
	}
	if len([]rune(name)) == 1 {
		if info, ok := input.CharKeyMap[[]rune(name)[0]]; ok {
			return info.Code, true
		}
	}
	return 0, false
}

// ─────────────────────────────────────────────────────────────────────────
// Per-key macro files
// ─────────────────────────────────────────────────────────────────────────

const macroDir = "macros"

func macroPath(slot uint16) string {
	return filepath.Join(macroDir, KeyName(slot)+".macro")
}

// saveMacro writes macroDir/<key>.macro in the:
//
//	delay=true/false
//
//	<body>
//
// format.
func saveMacro(slot uint16, delayEnabled bool, body string) error {
	if err := os.MkdirAll(macroDir, 0755); err != nil {
		return err
	}
	content := fmt.Sprintf("delay=%t\n\n%s\n", delayEnabled, body)
	return os.WriteFile(macroPath(slot), []byte(content), 0644)
}

// loadMacro reads and parses a macro file. Newlines in the body (added for
// human readability when hand-editing) are stripped back out.
func loadMacro(slot uint16) (delayEnabled bool, body string, err error) {
	f, err := os.Open(macroPath(slot))
	if err != nil {
		return false, "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return false, "", fmt.Errorf("empty macro file")
	}
	header := strings.TrimSpace(scanner.Text())
	delayEnabled = strings.EqualFold(strings.TrimPrefix(header, "delay="), "true")

	var b strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		b.WriteString(strings.TrimSpace(line))
	}
	return delayEnabled, b.String(), scanner.Err()
}

// openMacroFile opens the macro file for a slot in the user's default text
// editor/handler, creating an empty template first if it doesn't exist yet.
func openMacroFile(slot uint16) {
	path := macroPath(slot)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := saveMacro(slot, true, ""); err != nil {
			p.Plain("failed to create macro file:", err)
			return
		}
	}
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		p.Plain("failed to open editor (is xdg-open installed?):", err)
		p.Plain("macro file is at:", path)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Playback (superset of main.go's ExecuteAHKSequence)
// ─────────────────────────────────────────────────────────────────────────

// playState tracks whether a macro is currently running so Esc can abort it.
type playState struct {
	mu     sync.Mutex
	abort  chan struct{}
	Active bool
}

func (p *playState) Start() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.abort = make(chan struct{})
	p.Active = true
	return p.abort
}

func (p *playState) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Active = false
}

func (p *playState) Abort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Active {
		close(p.abort)
		p.Active = false
	}
}

var globalPlayState = &playState{}

// sleepInterruptible sleeps for d, or returns early (true) if abort fires.
func sleepInterruptible(d time.Duration, abort <-chan struct{}) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return false
	case <-abort:
		return true
	}
}

// PlayMacro plays back a macro body against send. If delayEnabled is false,
// {delay ...} blocks are skipped entirely. Playback stops immediately if
// abort fires (e.g. because Esc was pressed).
func PlayMacro(send *IMan.ManagerConnection, delayEnabled bool, sequence string, abort <-chan struct{}) {
	i := 0
	length := len(sequence)

	ctrlActive := false
	shiftActive := false
	aborted := false

	sendKey := func(code uint16, value int32) bool {
		if aborted {
			return true
		}
		select {
		case <-abort:
			aborted = true
			return true
		default:
		}
		if err := send.Send(IMan.WireEvent{Type: input.EV_KEY, Code: code, Value: value}); err != nil {
			p.Plain("macro send error:", err)
			return false
		}
		if err := send.Send(IMan.WireEvent{Type: input.EV_SYN, Code: 0, Value: 0}); err != nil {
			p.Plain("macro send error:", err)
			return false
		}
		if sleepInterruptible(10*time.Millisecond, abort) {
			aborted = true
			return true
		}
		return false
	}
	clearModifiers := func() {
		if ctrlActive {
			sendKey(input.KEY_LEFTCTRL, 0)
			ctrlActive = false
		}
		if shiftActive {
			sendKey(input.KEY_LEFTSHIFT, 0)
			shiftActive = false
		}
	}

	for i < length {
		if aborted {
			break
		}
		select {
		case <-abort:
			aborted = true
		default:
		}
		if aborted {
			break
		}

		char := string(sequence[i])

		switch char {
		case "^":
			if sendKey(input.KEY_LEFTCTRL, 1) {
				break
			}
			ctrlActive = true
			i++
		case "+":
			if sendKey(input.KEY_LEFTSHIFT, 1) {
				break
			}
			shiftActive = true
			i++
		case "{":
			endIdx := strings.Index(sequence[i:], "}")
			if endIdx == -1 {
				i++
				continue
			}
			blockContent := strings.ToLower(sequence[i+1 : i+endIdx])
			i += endIdx + 1

			parts := strings.Fields(blockContent)
			if len(parts) == 0 {
				continue
			}

			// {delay Nms}
			if parts[0] == "delay" && len(parts) >= 2 {
				if !delayEnabled {
					continue
				}
				d := strings.TrimSuffix(parts[1], "ms")
				if ms, err := strconv.Atoi(d); err == nil {
					if sleepInterruptible(time.Duration(ms)*time.Millisecond, abort) {
						aborted = true
					}
				}
				continue
			}

			keyName := parts[0]
			code, ok := KeyCodeByName(keyName)
			if !ok {
				p.Warn("WARNING: failed to find key", keyName)
				continue // unknown key name, skip
			}

			if len(parts) >= 2 {
				switch parts[1] {
				case "down":
					sendKey(code, 1)
				case "up":
					sendKey(code, 0)
				default:
					// {name N} -> tap N times
					if n, err := strconv.Atoi(parts[1]); err == nil {
						for r := 0; r < n && !aborted; r++ {
							sendKey(code, 1)
							sendKey(code, 0)
						}
					}
				}
			} else {
				// single tap macro like {left} or {a}
				keyLookup, hasShiftInfo := input.CharKeyMap[[]rune(keyName)[0]]
				newshift := false
				if len([]rune(keyName)) == 1 && hasShiftInfo && keyLookup.Shift && !shiftActive {
					newshift = true
					sendKey(input.KEY_LEFTSHIFT, 1)
				}
				sendKey(code, 1)
				sendKey(code, 0)
				if newshift {
					sendKey(input.KEY_LEFTSHIFT, 0)
				}
				clearModifiers()
			}

		default:
			key := input.CharKeyMap[rune(strings.ToLower(char)[0])]
			code := key.Code
			newshift := false
			if key.Shift && !shiftActive {
				newshift = true
				sendKey(input.KEY_LEFTSHIFT, 1)
			}
			sendKey(code, 1)
			sendKey(code, 0)
			if key.Shift && newshift {
				sendKey(input.KEY_LEFTSHIFT, 0)
			}
			clearModifiers()
			i++
		}
	}
	clearModifiers()
}

// ─────────────────────────────────────────────────────────────────────────
// Recorder + daemon
// ─────────────────────────────────────────────────────────────────────────

const modifierKey = input.KEY_RIGHTALT // hold this to arm record/play/edit triggers
const playKey = input.KEY_RIGHTSHIFT   // hold this to arm record/play/edit triggers

func isModifierCode(code uint16) bool {
	switch code {
	case modifierKey, input.KEY_LEFTCTRL, input.KEY_RIGHTCTRL, input.KEY_LEFTSHIFT, input.KEY_RIGHTSHIFT:
		return true
	}
	return false
}

var p = pp.New()

func main() {
	filterConn, err := IMan.Connect("macro-daemon", IMan.ModeFilter)
	if err != nil {
		panic(err)
	}
	sendConn, err := IMan.Connect("macro-daemon", IMan.ModeInjection)
	if err != nil {
		panic(err)
	}
	if err := filterConn.EnableKeyMap(false); err != nil {
		panic(err)
	}
	p.Plain("hello", 42, map[string]any{"a": 1, "b": []int{1, 2, 3}})
	p.Info("listening on", 8080) // gated by p.ShowInfo  (default: off)
	p.Error("request failed:", err)
	p.Success("deployment complete")

	var recMu sync.Mutex
	var recordingSlot uint16 = 0
	var recordBuf strings.Builder
	var lastEventTime time.Time
	var firstToken = true
	keysDownInMacro := map[uint16]bool{}

	p.Plainest("macro daemon running.")
	p.Plainest("  RightAlt + <key>            : start/stop recording macro on <key>")
	p.Plainest("  RightAlt + Shift + <key>     : play macro bound to <key>")
	p.Plainest("  LeftCtrl + RightAlt + <key>  : edit macro file for <key>")
	p.Plainest("  Esc                           : abort a playing macro")

	for {
		re, err := filterConn.ReadNext()
		if err != nil {
			panic(err)
		}
		ev := re.Event

		if ev.Type != input.EV_KEY {
			filterConn.BlockInput(0)
			continue
		}
		code := ev.Code

		modHeld := filterConn.IsPressed(modifierKey)
		// Esc aborts an in-flight macro, but otherwise behaves normally.
		if code == input.KEY_ESC && ev.Value == 1 {
			if modHeld && recordingSlot != 0 {
				recordingSlot = 0
				filterConn.BlockInput(1)
			} else {
				if globalPlayState.Active {
					filterConn.BlockInput(1)
				} else {
					filterConn.BlockInput(0)
				}
			}
			globalPlayState.Abort()
			continue
		}

		if isModifierCode(code) {
			filterConn.BlockInput(0)
			continue
		}

		if modHeld && ev.Value == 1 {
			shiftHeld := filterConn.IsPressed(playKey)
			ctrlHeld := filterConn.IsPressed(input.KEY_LEFTCTRL)
			filterConn.BlockInput(1) // swallow the trigger key itself

			switch {
			case ctrlHeld:
				p.Debug(fmt.Sprintf("opening macro file for %q\n", KeyName(code)))
				openMacroFile(code)

			case shiftHeld:
				delayEnabled, seq, err := loadMacro(code)
				if err != nil {
					fmt.Printf("no macro bound to %q\n", KeyName(code))
				} else {
					fmt.Printf("playing macro %q\n", KeyName(code))
					sendConn.Send(IMan.WireEvent{
						Code:  modifierKey,
						Type:  input.EV_KEY,
						Value: 0,
					})
					sendConn.Send(IMan.WireEvent{
						Code:  playKey,
						Type:  input.EV_KEY,
						Value: 0,
					})
					abort := globalPlayState.Start()
					go func() {
						defer globalPlayState.Stop()
						PlayMacro(sendConn, delayEnabled, seq, abort)
					}()
				}

			default:
				recMu.Lock()
				switch recordingSlot {
				case 0:
					recordingSlot = code
					recordBuf.Reset()
					firstToken = true
					keysDownInMacro = map[uint16]bool{}
					fmt.Printf("recording macro on %q...\n", KeyName(code))
				case code:
					if err := saveMacro(code, true, recordBuf.String()); err != nil {
						p.Plain("failed to save macro:", err)
					} else {
						fmt.Printf("saved macro %q: %s\n", KeyName(code), recordBuf.String())
					}
					recordingSlot = 0
				default:
					fmt.Printf("already recording %q, ignoring %q\n", KeyName(recordingSlot), KeyName(code))
				}
				recMu.Unlock()
			}
			continue
		}

		recMu.Lock()
		if recordingSlot != 0 && ev.Value != 2 { // skip autorepeat
			now := time.Now()

			if ev.Value == 1 {
				// key down: always record, and mark it as "owned" by this macro
				if !firstToken {
					if gap := now.Sub(lastEventTime); gap > 15*time.Millisecond {
						fmt.Fprintf(&recordBuf, "{delay %dms}", gap.Milliseconds())
					}
				}
				keysDownInMacro[code] = true
				fmt.Fprintf(&recordBuf, "{%s down}", KeyName(code))
				lastEventTime = now
				firstToken = false
			} else {
				// key up: only record it if this macro actually recorded the
				// matching down -- avoids leading "up" noise from keys that
				// were already held (e.g. modifiers) before recording started.
				if keysDownInMacro[code] {
					if !firstToken {
						if gap := now.Sub(lastEventTime); gap > 15*time.Millisecond {
							fmt.Fprintf(&recordBuf, "{delay %dms}", gap.Milliseconds())
						}
					}
					delete(keysDownInMacro, code)
					fmt.Fprintf(&recordBuf, "{%s up}", KeyName(code))
					lastEventTime = now
					firstToken = false
				} else {
					// swallow silently, but resync the clock so the skipped
					// time doesn't get charged to the next real delay
					lastEventTime = now
				}
			}
		}
		recMu.Unlock()

		filterConn.BlockInput(0)
	}
}
