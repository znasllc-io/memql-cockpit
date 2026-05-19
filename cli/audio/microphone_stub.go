//go:build !voice

// Stub for builds without the `voice` tag. Returns a clear error so
// the chat view can render a "voice not built into this binary"
// status instead of crashing on first PTT keypress.
package audio

import (
	"errors"
	"io"
)

// Format mirrors microphone.go's Format so callers don't need
// build-tagged code at the use site.
type Format struct {
	Encoding   string
	SampleRate uint32
	Channels   uint32
}

func DefaultFormat() Format {
	return Format{Encoding: "pcm16", SampleRate: 16000, Channels: 1}
}

var errVoiceNotBuilt = errors.New("audio: voice support not compiled in (rebuild with `go build -tags voice` or `make cockpit-voice`)")

func StartCapture(Format) (io.Reader, func() error, error) {
	return nil, nil, errVoiceNotBuilt
}
