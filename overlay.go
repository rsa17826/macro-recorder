package main

import (
	"image/color"
	"sync"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// ─────────────────────────────────────────────────────────────────────────
// Shared overlay state — updated by the daemon loop / PlayMacro, read by
// the GUI goroutine each frame.
// ─────────────────────────────────────────────────────────────────────────

type overlayMode int

const (
	modeIdle overlayMode = iota
	modeRecording
	modePlaying
)

type overlayStateT struct {
	mu sync.Mutex

	mode overlayMode

	// recording
	recordSlot string // key name the macro is bound to
	recordLast string // e.g. "d down", "delay 1000ms"

	// playing
	playSlot                    string // key name the macro is bound to
	playPrev, playCur, playNext string
}

var overlay = &overlayStateT{}

// overlayChanged wakes the manager/window goroutines whenever state changes.
var overlayChanged = make(chan struct{}, 1)

func notifyOverlay() {
	select {
	case overlayChanged <- struct{}{}:
	default:
	}
}

func setIdle() {
	overlay.mu.Lock()
	overlay.mode = modeIdle
	overlay.mu.Unlock()
	notifyOverlay()
}

func setRecording(slot, last string) {
	overlay.mu.Lock()
	overlay.mode = modeRecording
	overlay.recordSlot = slot
	overlay.recordLast = last
	overlay.mu.Unlock()
	notifyOverlay()
}

func setPlaying(slot, prev, cur, next string) {
	overlay.mu.Lock()
	overlay.mode = modePlaying
	overlay.playSlot = slot
	overlay.playPrev = prev
	overlay.playCur = cur
	overlay.playNext = next
	overlay.mu.Unlock()
	notifyOverlay()
}

func snapshot() overlayStateT {
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	return overlayStateT{
		mode:       overlay.mode,
		recordSlot: overlay.recordSlot,
		recordLast: overlay.recordLast,
		playSlot:   overlay.playSlot,
		playPrev:   overlay.playPrev,
		playCur:    overlay.playCur,
		playNext:   overlay.playNext,
	}
}

func snapshotMode() overlayMode {
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	return overlay.mode
}

// ─────────────────────────────────────────────────────────────────────────
// Window lifecycle manager — opens a window only while mode != idle, and
// closes it (not just blanks it) the moment we go back to idle.
// ─────────────────────────────────────────────────────────────────────────

// runGUI starts the manager goroutine. Call from a real main() like:
//
//	func main() {
//	    runGUI()
//	    go runDaemon()
//	    app.Main()
//	}
func runGUI() {
	go guiManager()
}

func guiManager() {
	var (
		open       bool
		redrawChan chan struct{} // forwarded to the active window's loop
		closedChan chan struct{} // closed by the window loop when it exits
	)

	for range overlayChanged {
		st := snapshotMode()

		if st != modeIdle && !open {
			open = true
			redrawChan = make(chan struct{}, 1)
			closedChan = make(chan struct{})
			go func(redraw <-chan struct{}, closed chan<- struct{}) {
				defer close(closed)
				if err := openWindow(redraw); err != nil {
					p.Error("gui error:", err)
				}
			}(redrawChan, closedChan)
			continue
		}

		if st == modeIdle && open {
			select {
			case redrawChan <- struct{}{}: // wake it so it notices idle
			default:
			}
			closeActiveWindow()
			<-closedChan
			open = false
			continue
		}

		if open {
			select {
			case redrawChan <- struct{}{}:
			default:
			}
		}
	}
}

// activeWindow lets guiManager ask the currently-open window to close.
var (
	activeWindowMu sync.Mutex
	activeWindow   *app.Window
)

func closeActiveWindow() {
	activeWindowMu.Lock()
	w := activeWindow
	activeWindowMu.Unlock()
	if w != nil {
		w.Perform(system.ActionClose)
	}
}

func openWindow(redraw <-chan struct{}) error {
	w := new(app.Window)
	w.Option(
		app.Title("macro"),
		app.Size(unit.Dp(420), unit.Dp(160)),
		app.Decorated(false),
		app.MinSize(unit.Dp(420), unit.Dp(160)),
		app.MaxSize(unit.Dp(420), unit.Dp(160)),
	)

	activeWindowMu.Lock()
	activeWindow = w
	activeWindowMu.Unlock()
	defer func() {
		activeWindowMu.Lock()
		activeWindow = nil
		activeWindowMu.Unlock()
	}()

	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	var ops op.Ops

	events := make(chan any)
	acks := make(chan struct{})
	go func() {
		for {
			e := w.Event()
			events <- e
			<-acks
			if _, ok := e.(app.DestroyEvent); ok {
				return
			}
		}
	}()

	for {
		select {
		case e := <-events:
			switch e := e.(type) {
			case app.DestroyEvent:
				acks <- struct{}{}
				return e.Err
			case app.FrameEvent:
				gtx := app.NewContext(&ops, e)
				st := snapshot()
				if st.mode == modeIdle {
					// Mode flipped back to idle between the close request
					// and this frame; bail instead of drawing a stale frame.
					w.Perform(system.ActionClose)
				}
				drawOverlay(gtx, th, st)
				e.Frame(gtx.Ops)
				acks <- struct{}{}
			default:
				acks <- struct{}{}
			}
		case <-redraw:
			if snapshotMode() == modeIdle {
				w.Perform(system.ActionClose)
			} else {
				w.Invalidate()
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Drawing
// ─────────────────────────────────────────────────────────────────────────

var (
	colBG   = color.NRGBA{R: 0x18, G: 0x18, B: 0x1c, A: 0xf0}
	colMain = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	colDim  = color.NRGBA{R: 0x90, G: 0x90, B: 0x98, A: 0xff}
	colRec  = color.NRGBA{R: 0xff, G: 0x55, B: 0x55, A: 0xff}
)

func drawOverlay(gtx layout.Context, th *material.Theme, st overlayStateT) {
	paint.FillShape(gtx.Ops, colBG, clip.Rect{Max: gtx.Constraints.Max}.Op())

	switch st.mode {
	case modeIdle:
		return

	case modeRecording:
		layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th, unit.Sp(14), "● RECORDING")
				lbl.Color = colRec
				lbl.Alignment = text.Middle
				return layout.Center.Layout(gtx, lbl.Layout)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th, unit.Sp(36), st.recordSlot)
				lbl.Color = colMain
				lbl.Alignment = text.Middle
				lbl.Font.Weight = 700
				return layout.Center.Layout(gtx, lbl.Layout)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th, unit.Sp(16), st.recordLast)
				lbl.Color = colDim
				lbl.Alignment = text.Middle
				return layout.Center.Layout(gtx, lbl.Layout)
			}),
		)

	case modePlaying:
		layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th, unit.Sp(14), "▶ PLAYING "+st.playSlot)
				lbl.Color = colMain
				lbl.Alignment = text.Middle
				return layout.Center.Layout(gtx, lbl.Layout)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Center.Layout(gtx, keyLabel(th, st.playPrev, unit.Sp(16), colDim).Layout)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Center.Layout(gtx, keyLabel(th, st.playCur, unit.Sp(26), colMain).Layout)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Center.Layout(gtx, keyLabel(th, st.playNext, unit.Sp(16), colDim).Layout)
					}),
				)
			}),
		)
	}
}

func keyLabel(th *material.Theme, s string, size unit.Sp, col color.NRGBA) material.LabelStyle {
	if s == "" {
		s = "·"
	}
	lbl := material.Label(th, size, s)
	lbl.Color = col
	lbl.Alignment = text.Middle
	lbl.MaxLines = 1
	return lbl
}
