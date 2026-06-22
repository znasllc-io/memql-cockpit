//go:build voice

// Package audio captures microphone audio for the cockpit's push-to-
// talk flow. Backed by malgo (miniaudio) -- pure-Go-friendly CGO
// bindings to a single-header audio lib, no system audio package
// required on the build host.
//
// Build tag `voice` gates this file so the default headless build
// stays CGO-free for cross-compilation. Build with
//
//	go build -tags voice
//
// to enable real capture. Without the tag, the stub in
// microphone_stub.go returns an error from StartCapture so the chat
// view can surface a clear "voice not built into this binary"
// message instead of crashing.
package audio

import (
	"fmt"
	"io"
	"sync"

	"github.com/gen2brain/malgo"
)

// Format describes the PCM format StartCapture configures the device
// to capture in. Matches the shape memql-sdk-go's voice package
// accepts for AiTranscribeStreamStart.
type Format struct {
	Encoding   string // always "pcm16" for now
	SampleRate uint32
	Channels   uint32
}

// DefaultFormat is the recommended capture format: 16kHz mono PCM16,
// matched against OpenAI Realtime's expected input. Caller can
// override.
func DefaultFormat() Format {
	return Format{Encoding: "pcm16", SampleRate: 16000, Channels: 1}
}

// StartCapture opens the default input device with the supplied
// format and returns a Reader over the captured audio bytes plus a
// stop function the caller must invoke when the session ends.
//
// The Reader is backed by a bounded ring; if the consumer falls
// behind, oldest bytes are dropped to bound memory. Production code
// should drain promptly (the SDK's voice.PushToTalk consumes the
// reader in a dedicated goroutine and forwards bytes immediately
// over gRPC, which is fine).
func StartCapture(fmtSpec Format) (io.Reader, func() error, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("audio: malgo InitContext: %w", err)
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = fmtSpec.Channels
	cfg.SampleRate = fmtSpec.SampleRate
	cfg.Alsa.NoMMap = 1

	pipe := newRingReader(1 << 17) // 128 KB ring; ~2s of 16kHz mono PCM16

	deviceCallbacks := malgo.DeviceCallbacks{
		Data: func(_, in []byte, _ uint32) {
			pipe.write(in)
		},
	}

	device, err := malgo.InitDevice(ctx.Context, cfg, deviceCallbacks)
	if err != nil {
		_ = ctx.Uninit()
		ctx.Free()
		return nil, nil, fmt.Errorf("audio: InitDevice: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		_ = ctx.Uninit()
		ctx.Free()
		return nil, nil, fmt.Errorf("audio: device.Start: %w", err)
	}

	// stop is idempotent: the chat view calls it once on the second
	// `v` press to drive the SDK toward EOF + clean End{cancel:false},
	// then again from the PushToTalk-pump goroutine's defer after
	// Complete lands. malgo's Free() is not safe to call twice, so
	// guard with sync.Once.
	var (
		stopOnce sync.Once
		stopErr  error
	)
	stop := func() error {
		stopOnce.Do(func() {
			device.Uninit()
			if err := ctx.Uninit(); err != nil {
				ctx.Free()
				stopErr = fmt.Errorf("audio: ctx.Uninit: %w", err)
				pipe.close()
				return
			}
			ctx.Free()
			pipe.close()
		})
		return stopErr
	}
	return pipe, stop, nil
}
