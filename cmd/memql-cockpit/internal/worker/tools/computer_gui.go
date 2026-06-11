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
// window_list / window_focus are deliberately absent: they're
// unsupported_on_platform stubs today (see dispatchComputer) and
// must not be advertised until they ship for real. The
// `capabilities` action is also absent -- it's build-agnostic and
// routed by the dispatcher before this table is consulted.
var computerActionHandlers = map[string]func(map[string]any) (*memqlv1.Success, *memqlv1.Failure){
	"screenshot":      guiScreenshot,
	"cursor_position": guiCursorPosition,
	"mouse_move":      guiMouseMove,
	"mouse_click":     guiMouseClick,
	"mouse_drag":      guiMouseDrag,
	"mouse_scroll":    guiMouseScroll,
	"key_type":        guiKeyType,
	"key_combo":       guiKeyCombo,
	"display_info":    guiDisplayInfo,
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
	switch action {
	case "window_list":
		return guiWindowList(args)
	case "window_focus":
		return guiWindowFocus(args)
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
	// Per-call TCC preflight: confirm the Screen Recording grant is
	// still in place. The setup wizard probes this once at first
	// run; if the user later revokes the grant from System Settings
	// -> Privacy -> Screen Recording, the next screenshot would
	// otherwise return an empty / black image with no clear error.
	// The hook is set by the darwin && gui build at init time; on
	// every other platform it's nil and we skip.
	if ScreenCapturePreflightHook != nil && !ScreenCapturePreflightHook() {
		return nil, failure("permission_denied",
			"Screen Recording permission is not granted for this binary. "+
				"Open System Settings -> Privacy & Security -> Screen Recording, "+
				"enable memql-cockpit-gui, then re-run.")
	}
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
	double := argBool(args, "double", false)
	if argInt(args, "count", 1) >= 2 {
		double = true
	}
	if err := robotgo.Click(button, double); err != nil {
		return nil, failure("mouse_click_failed", err.Error())
	}
	return successComputerJSON(map[string]any{"button": button, "double": double}, "clicked", 0), nil
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

func guiDisplayInfo(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	width, height := robotgo.GetScreenSize()
	return successComputerJSON(map[string]any{
		"width":  width,
		"height": height,
	}, fmt.Sprintf("%dx%d", width, height), 0), nil
}

// guiWindowList and guiWindowFocus depend on robotgo's Window
// helpers; coverage varies by platform (full on macOS via
// applescript; partial on Linux). MVP returns "unsupported" on
// platforms where the window API isn't reliable; the slot stays
// here so future versions can wire in window enumeration without
// touching the dispatcher.
func guiWindowList(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	return nil, &memqlv1.Failure{
		ErrorCode:    "unsupported_on_platform",
		ErrorMessage: "window_list not yet supported on this build; track via Phase 7+ window-API polish",
	}
}

func guiWindowFocus(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	_ = args
	return nil, &memqlv1.Failure{
		ErrorCode:    "unsupported_on_platform",
		ErrorMessage: "window_focus not yet supported on this build; track via Phase 7+ window-API polish",
	}
}

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
