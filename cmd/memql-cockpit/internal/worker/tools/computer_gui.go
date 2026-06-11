//go:build gui

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/png" // register the PNG decoder for image.Decode (SaveCapture writes PNG)
	"os"
	"strings"

	"github.com/go-vgo/robotgo"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// computerActionHandlers is the single source of truth for the
// GUI-backed workerComputer actions this build supports. The
// dispatcher routes through it AND the capability descriptor
// (capabilities_gui.go) derives its advertised action list from it,
// so the two can never drift (memql-cockpit#162).
//
// The `capabilities` and `wait` actions are absent -- they are
// build-agnostic and routed by the dispatcher before this table is
// consulted (capabilities.go / wait.go).
//
// window_list / window_focus (memql-cockpit#167) are real on macOS
// (CGWindowList) and Linux/X11 (EWMH); on Wayland and other gui
// platforms their handlers return a structured
// unsupported_on_platform failure -- consumers should read the
// descriptor's displayServer field alongside the action list.
var computerActionHandlers = map[string]func(map[string]any) (*memqlv1.Success, *memqlv1.Failure){
	"screenshot":      guiScreenshot,
	"cursor_position": guiCursorPosition,
	"mouse_move":      guiMouseMove,
	"mouse_click":     guiMouseClick,
	"mouse_down":      guiMouseDown,
	"mouse_up":        guiMouseUp,
	"mouse_drag":      guiMouseDrag,
	"mouse_scroll":    guiMouseScroll,
	"key_type":        guiKeyType,
	"key_combo":       guiKeyCombo,
	"key_hold":        guiKeyHold,
	"display_info":    guiDisplayInfo,
	"window_list":     guiWindowList,
	"window_focus":    guiWindowFocus,
}

// dispatchComputer routes workerComputer.<action> to the matching
// RobotGo-backed implementation. The default-build sibling
// (computer.go, //go:build !gui) returns Unimplemented.
//
// Handlers run on the cockpit-gui process; macOS TCC permissions
// (Accessibility for input, Screen Recording for screenshot) are
// required and probed at startup -- see worker.PermissionStatus
// and `memql-cockpit-gui worker setup` for the operator wizard.
func (d *Dispatcher) dispatchComputer(ctx context.Context, action string, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	_ = ctx
	if handler, ok := computerActionHandlers[action]; ok {
		return handler(args)
	}
	return nil, &memqlv1.Failure{
		ErrorCode:    "unknown_action",
		ErrorMessage: fmt.Sprintf("workerComputer.%s not implemented", action),
	}
}

// guiScreenshot captures the screen (or a region when supplied)
// and returns base64-encoded image bytes plus dimensions.
//
// Args:
//
//	format: "png" (default) | "jpeg"
//	region: { x, y, w, h } -- optional sub-rect (logical coords); full screen when absent
//	quality: int 1-100 (jpeg only; default 80)
//	maxLongEdge: int, downscale ceiling for the emitted image's long
//	  edge (default 1568, clamped to [512, 8000])
//
// Coordinate contract (see coords.go): the emitted image defines the
// space the model speaks. width/height are the emitted dims,
// sourceWidth/sourceHeight the captured physical dims, scale the
// emitted/captured ratio, logicalWidth/logicalHeight the RobotGo
// input-space dims (the requested w/h for region captures).
func guiScreenshot(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	// The per-call Screen Recording TCC preflight runs in the
	// dispatcher's preflightComputerAction (preflight.go) before this
	// handler is reached, alongside the Accessibility + display-server
	// gates (memql-cockpit#164).
	format := strings.ToLower(strings.TrimSpace(argString(args, "format")))
	if format == "" {
		format = "png"
	}
	if format != "png" && format != "jpeg" && format != "jpg" {
		return nil, failure("bad_request", fmt.Sprintf("screenshot format %q not supported (use png or jpeg)", format))
	}
	region := argMap(args, "region")
	// Capture always lands as PNG; the requested output format is
	// applied at re-encode time below.
	tmp, err := os.CreateTemp("", "worker-screenshot-*.png")
	if err != nil {
		return nil, failure("screenshot_failed", err.Error())
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	// Logical (input-space) dims for the payload: full screen uses
	// the RobotGo screen size; a region capture is relative to the
	// requested logical rect.
	logicalW, logicalH := robotgo.GetScreenSize()
	if region != nil {
		x := argInt(region, "x", 0)
		y := argInt(region, "y", 0)
		w := argInt(region, "w", 0)
		h := argInt(region, "h", 0)
		if w <= 0 || h <= 0 {
			return nil, failure("bad_request", "screenshot region requires positive w + h")
		}
		if err := robotgo.SaveCapture(tmpPath, x, y, w, h); err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}
		logicalW, logicalH = w, h
	} else {
		if err := robotgo.SaveCapture(tmpPath); err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}
	}

	rawCapture, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, failure("screenshot_failed", err.Error())
	}

	// Decode to learn the true captured (physical) dimensions -- on
	// Retina/HiDPI hosts these exceed the logical size -- then apply
	// the downscale policy so the emitted image stays vision-friendly.
	img, _, err := image.Decode(bytes.NewReader(rawCapture))
	if err != nil {
		return nil, failure("screenshot_failed", "decode capture: "+err.Error())
	}
	capturedW, capturedH := img.Bounds().Dx(), img.Bounds().Dy()
	maxLongEdge := clampLongEdge(argInt(args, "maxLongEdge", defaultMaxLongEdge))
	emittedW, emittedH := FitWithin(capturedW, capturedH, maxLongEdge)
	if emittedW != capturedW || emittedH != capturedH {
		img = downscaleImage(img, emittedW, emittedH)
	}

	raw, err := encodeImage(img, format, argInt(args, "quality", 80))
	if err != nil {
		return nil, failure("screenshot_failed", "encode: "+err.Error())
	}

	scale := float64(emittedW) / float64(capturedW)
	encoded := base64.StdEncoding.EncodeToString(raw)
	preview := fmt.Sprintf("[%s] %dx%d (source %dx%d, scale %.3f), %d bytes",
		format, emittedW, emittedH, capturedW, capturedH, scale, len(raw))
	payload := map[string]any{
		"format":        format,
		"width":         emittedW,
		"height":        emittedH,
		"sourceWidth":   capturedW,
		"sourceHeight":  capturedH,
		"scale":         scale,
		"logicalWidth":  logicalW,
		"logicalHeight": logicalH,
		"bytesBase64":   encoded,
		"sizeBytes":     len(raw),
	}
	if region != nil {
		payload["region"] = map[string]any{
			"x": argInt(region, "x", 0),
			"y": argInt(region, "y", 0),
			"w": argInt(region, "w", 0),
			"h": argInt(region, "h", 0),
		}
	}
	return successComputerJSON(payload, preview, len(raw)), nil
}

func guiCursorPosition(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x, y := robotgo.Location()
	return successComputerJSON(map[string]any{"x": x, "y": y}, fmt.Sprintf("(%d,%d)", x, y), 0), nil
}

// cursorLocation reports the live cursor position for the consent
// gate's strict-mode region exemption (memql-cockpit#131). The
// dispatcher calls it just before gating a workerComputer.mouse_click
// so an in-region click can skip the per-action approval modal.
// Always Known=true on the GUI build; the headless stub returns false.
func cursorLocation() (x, y int, known bool) {
	x, y = robotgo.Location()
	return x, y, true
}

// liveMapper builds the coordinate mapper for the main display from
// live geometry plus the fixed downscale policy (defaultMaxLongEdge),
// per the contract in coords.go: model coords are in the emitted
// screenshot space, and the mapping to logical input coords is
// recomputed statelessly on every mouse action.
//
// Physical dims derive from robotgo.ScaleF() (2.0 on Retina macOS).
// Defensive: a non-positive scale is treated as 1, so on displays
// where capture dims equal logical dims the mapper is an identity
// (modulo the downscale policy).
func liveMapper() CoordinateMapper {
	logicalW, logicalH := robotgo.GetScreenSize()
	scale := robotgo.ScaleF()
	if scale <= 0 {
		scale = 1
	}
	capturedW := roundHalfAway(float64(logicalW) * scale)
	capturedH := roundHalfAway(float64(logicalH) * scale)
	emittedW, emittedH := FitWithin(capturedW, capturedH, defaultMaxLongEdge)
	return NewCoordinateMapper(logicalW, logicalH, capturedW, capturedH, emittedW, emittedH)
}

func guiMouseMove(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x := argInt(args, "x", -1)
	y := argInt(args, "y", -1)
	if x < 0 || y < 0 {
		return nil, failure("bad_request", "mouse_move: x and y are required and must be non-negative")
	}
	m := liveMapper()
	if fail := validatePointInRect("mouse_move", x, y, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	lx, ly := m.ToLogical(x, y)
	smooth := argBool(args, "smooth", false)
	if smooth {
		robotgo.MoveSmooth(lx, ly)
	} else {
		robotgo.Move(lx, ly)
	}
	return successComputerJSON(map[string]any{
		"x": x, "y": y,
		"logicalX": lx, "logicalY": ly,
		"smooth": smooth,
	}, "moved", 0), nil
}

func guiMouseClick(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button := strings.ToLower(strings.TrimSpace(argString(args, "button")))
	if button == "" {
		button = "left"
	}
	// count drives single / double / triple; the legacy `double` flag
	// is honored as count=2 for back-compat. Clamped to [1, 3] --
	// nothing in the UI vocabulary needs more than a triple-click.
	count := clampInt(argInt(args, "count", 1), 1, 3)
	if argBool(args, "double", false) && count < 2 {
		count = 2
	}
	var err error
	switch count {
	case 2:
		err = robotgo.Click(button, true)
	case 3:
		// robotgo.Click only knows single/double. MultiClick(.., 3)
		// posts a proper clickCount=3 event sequence on macOS
		// (CGEventSetIntegerValueField under the hood) and three
		// rapid clicks elsewhere, which X11 coalesces into a triple.
		err = robotgo.MultiClick(button, 3)
	default:
		err = robotgo.Click(button, false)
	}
	if err != nil {
		return nil, failure("mouse_click_failed", err.Error())
	}
	return successComputerJSON(map[string]any{
		"button": button,
		"count":  count,
		"double": count >= 2, // legacy field, kept for payload back-compat
	}, "clicked", 0), nil
}

// guiMouseDown / guiMouseUp post a bare button-state transition
// (memql-cockpit#166) for press-and-hold interactions the composite
// mouse_click / mouse_drag can't express (e.g. hold-to-reveal menus,
// custom drag handles needing intermediate moves). Deliberately
// STATELESS: mouse_up succeeds even with no preceding mouse_down --
// the OS treats a redundant button-up as a no-op, and tracking
// pairing here would just desync from reality on any missed event.
func guiMouseDown(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button, fail := mouseButtonArg(args, "mouse_down")
	if fail != nil {
		return nil, fail
	}
	if err := robotgo.MouseDown(button); err != nil {
		return nil, failure("mouse_down_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"button": button}, "mouse down", 0), nil
}

func guiMouseUp(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	button, fail := mouseButtonArg(args, "mouse_up")
	if fail != nil {
		return nil, fail
	}
	if err := robotgo.MouseUp(button); err != nil {
		return nil, failure("mouse_up_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"button": button}, "mouse up", 0), nil
}

// mouseButtonArg validates the {button} argument for mouse_down /
// mouse_up. Vocabulary is left / right / middle (defaulting to
// left); "middle" maps to robotgo's "center" -- robotgo's CheckMouse
// silently falls back to LEFT_BUTTON for unknown names, so an
// unvalidated typo would click the wrong button instead of erroring.
func mouseButtonArg(args map[string]any, action string) (string, *memqlv1.Failure) {
	button := strings.ToLower(strings.TrimSpace(argString(args, "button")))
	switch button {
	case "", "left":
		return "left", nil
	case "right":
		return "right", nil
	case "middle", "center":
		return "center", nil
	}
	return "", failure("bad_request", fmt.Sprintf("%s: button %q not supported (use left, right, or middle)", action, button))
}

func guiMouseDrag(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	fromX := argInt(args, "fromX", -1)
	fromY := argInt(args, "fromY", -1)
	toX := argInt(args, "toX", -1)
	toY := argInt(args, "toY", -1)
	if fromX < 0 || fromY < 0 || toX < 0 || toY < 0 {
		return nil, failure("bad_request", "mouse_drag: fromX, fromY, toX, toY all required")
	}
	m := liveMapper()
	if fail := validatePointInRect("mouse_drag from", fromX, fromY, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	if fail := validatePointInRect("mouse_drag to", toX, toY, m.EmittedW, m.EmittedH); fail != nil {
		return nil, fail
	}
	lFromX, lFromY := m.ToLogical(fromX, fromY)
	lToX, lToY := m.ToLogical(toX, toY)
	robotgo.Move(lFromX, lFromY)
	robotgo.MilliSleep(50)
	robotgo.DragSmooth(lToX, lToY)
	return successComputerJSON(map[string]any{
		"fromX": fromX, "fromY": fromY,
		"toX": toX, "toY": toY,
		"logicalFromX": lFromX, "logicalFromY": lFromY,
		"logicalToX": lToX, "logicalToY": lToY,
	}, "dragged", 0), nil
}

// guiMouseScroll takes wheel-tick deltas (dx/dy), not screen
// coordinates, so the screenshot-space -> logical mapping that
// applies to mouse_move / mouse_drag targets does not apply here.
func guiMouseScroll(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	dx := argInt(args, "dx", 0)
	dy := argInt(args, "dy", 0)
	if dx == 0 && dy == 0 {
		return nil, failure("bad_request", "mouse_scroll: at least one of dx / dy must be non-zero")
	}
	robotgo.Scroll(dx, dy)
	return successComputerJSON(map[string]any{"dx": dx, "dy": dy}, "scrolled", 0), nil
}

func guiKeyType(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	text := argString(args, "text")
	if text == "" {
		return nil, failure("bad_request", "key_type: text required")
	}
	robotgo.TypeStr(text)
	// The typed text often includes credentials (the agent will
	// drive `key_type(text="<password>\n")` against a password
	// field). The dispatcher's audit log records the success preview
	// verbatim, which means the password would otherwise land in the
	// log shipper as cleartext. We deliberately omit the typed text
	// from both the result payload and the preview; the length is
	// preserved as a length-only indicator. If a downstream consumer
	// needs the typed value (e.g. a record-and-replay tool), it must
	// supply the value itself and not depend on read-back.
	return successComputerJSON(
		map[string]any{"chars": len(text), "text_redacted": true},
		fmt.Sprintf("typed %d chars (text redacted)", len(text)),
		len(text),
	), nil
}

func guiKeyCombo(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	keys := argStringList(args, "keys")
	if len(keys) == 0 {
		return nil, failure("bad_request", "key_combo: keys array required")
	}
	primary := keys[0]
	mods := make([]interface{}, 0, len(keys)-1)
	for _, k := range keys[1:] {
		mods = append(mods, k)
	}
	if err := robotgo.KeyTap(primary, mods...); err != nil {
		return nil, failure("key_combo_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"keys": keys}, "key combo", 0), nil
}

// keyHoldMaxMs bounds workerComputer.key_hold so a single dispatch
// can't pin a key (and the worker's call slot) indefinitely.
const keyHoldMaxMs = 10000

// guiKeyHold presses a key, holds it for durationMs, and releases it
// (memql-cockpit#166) -- the press-and-hold primitive key_press /
// key_combo can't express (games, OS-level hold gestures, key-repeat
// driven UIs). durationMs clamps into [1, keyHoldMaxMs]; absent
// clamps up from 0 to the 1ms floor (an instantaneous down/up).
//
// robotgo v1.0.2: KeyDown(key) == KeyToggle(key) (default direction
// "down") and KeyUp(key) == KeyToggle(key, "up") -- both return the
// underlying toggle error, so this is the dependable down/sleep/up
// path. On a failed release we still report key_hold_failed and name
// the release stage: a stuck key is operator-visible information.
func guiKeyHold(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	key := strings.TrimSpace(argString(args, "key"))
	if key == "" {
		return nil, failure("bad_request", "key_hold: key required")
	}
	durationMs := clampInt(argInt(args, "durationMs", 0), 1, keyHoldMaxMs)
	if err := robotgo.KeyDown(key); err != nil {
		return nil, failure("key_hold_failed", "press: "+err.Error())
	}
	robotgo.MilliSleep(durationMs)
	if err := robotgo.KeyUp(key); err != nil {
		return nil, failure("key_hold_failed", "release (key may be stuck down): "+err.Error())
	}
	return successComputerJSON(map[string]any{
		"key":        key,
		"durationMs": durationMs,
	}, fmt.Sprintf("held %q for %dms", key, durationMs), 0), nil
}

func guiDisplayInfo(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	width, height := robotgo.GetScreenSize()
	return successComputerJSON(map[string]any{
		"width":  width,
		"height": height,
	}, fmt.Sprintf("%dx%d", width, height), 0), nil
}

// guiWindowList / guiWindowFocus live in window_gui.go with their
// platform backends in window_gui_{darwin,linux,other}.go
// (memql-cockpit#167).

// successComputerJSON moved to capabilities.go (untagged) so the
// build-agnostic capabilities handler can share it.

// argBool extracts a boolean argument with a default. The args map
// may carry the value as a real bool (parsed JSON) or a string
// ("true" / "false") depending on the caller.
func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true")
	}
	return def
}
