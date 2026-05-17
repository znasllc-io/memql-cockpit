//go:build gui

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/go-vgo/robotgo"
)

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
	switch action {
	case "screenshot":
		return guiScreenshot(args)
	case "cursor_position":
		return guiCursorPosition(args)
	case "mouse_move":
		return guiMouseMove(args)
	case "mouse_click":
		return guiMouseClick(args)
	case "mouse_drag":
		return guiMouseDrag(args)
	case "mouse_scroll":
		return guiMouseScroll(args)
	case "key_type":
		return guiKeyType(args)
	case "key_combo":
		return guiKeyCombo(args)
	case "display_info":
		return guiDisplayInfo(args)
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
//	region: { x, y, w, h } -- optional sub-rect; full screen when absent
//	quality: int 1-100 (jpeg only; default 80)
func guiScreenshot(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	format := strings.ToLower(strings.TrimSpace(argString(args, "format")))
	if format == "" {
		format = "png"
	}
	region := argMap(args, "region")
	tmp, err := os.CreateTemp("", "worker-screenshot-*."+format)
	if err != nil {
		return nil, failure("screenshot_failed", err.Error())
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

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
	} else {
		if err := robotgo.SaveCapture(tmpPath); err != nil {
			return nil, failure("screenshot_failed", err.Error())
		}
	}

	raw, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, failure("screenshot_failed", err.Error())
	}

	// SaveCapture writes PNG by default; transcode to JPEG when
	// requested for smaller payloads on big screens.
	if format == "jpeg" || format == "jpg" {
		quality := argInt(args, "quality", 80)
		if quality < 1 {
			quality = 1
		} else if quality > 100 {
			quality = 100
		}
		raw, err = transcodeToJPEG(raw, quality)
		if err != nil {
			return nil, failure("screenshot_failed", "jpeg encode: "+err.Error())
		}
	}

	width, height := robotgo.GetScreenSize()
	encoded := base64.StdEncoding.EncodeToString(raw)
	preview := fmt.Sprintf("[%s] %dx%d, %d bytes", format, width, height, len(raw))
	return successComputerJSON(map[string]any{
		"format":      format,
		"width":       width,
		"height":      height,
		"sourceWidth":  width,
		"sourceHeight": height,
		"bytesBase64": encoded,
		"sizeBytes":   len(raw),
	}, preview, len(raw)), nil
}

func guiCursorPosition(_ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x, y := robotgo.Location()
	return successComputerJSON(map[string]any{"x": x, "y": y}, fmt.Sprintf("(%d,%d)", x, y), 0), nil
}

func guiMouseMove(args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	x := argInt(args, "x", -1)
	y := argInt(args, "y", -1)
	if x < 0 || y < 0 {
		return nil, failure("bad_request", "mouse_move: x and y are required and must be non-negative")
	}
	smooth := argBool(args, "smooth", false)
	if smooth {
		robotgo.MoveSmooth(x, y)
	} else {
		robotgo.Move(x, y)
	}
	return successComputerJSON(map[string]any{"x": x, "y": y, "smooth": smooth}, "moved", 0), nil
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
	robotgo.Move(fromX, fromY)
	robotgo.MilliSleep(50)
	robotgo.DragSmooth(toX, toY)
	return successComputerJSON(map[string]any{
		"fromX": fromX, "fromY": fromY,
		"toX": toX, "toY": toY,
	}, "dragged", 0), nil
}

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
	return successComputerJSON(map[string]any{"text": text, "chars": len(text)}, "typed", len(text)), nil
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

// successComputerJSON is a small helper kept here for the GUI
// handlers above.
func successComputerJSON(payload map[string]any, preview string, bytesOut int) *memqlv1.Success {
	body, _ := json.Marshal(payload)
	return &memqlv1.Success{
		ResultJson:    body,
		BytesOut:      uint64(bytesOut),
		OutputPreview: clampPreview(preview),
	}
}

func transcodeToJPEG(in []byte, quality int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(in))
	if err != nil {
		// SaveCapture default is PNG, so try png first if the
		// generic decoder doesn't recognize the magic bytes.
		img, err = png.Decode(bytes.NewReader(in))
		if err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

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
