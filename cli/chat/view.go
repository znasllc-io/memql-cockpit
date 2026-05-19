// Package chat renders the Chat tab: a polling viewer over the
// single-chat-per-space utterance stream.
//
// One pane per axis: left lists active spaces (v1:cognition:space
// rows), right shows the most-recent utterances for the selected
// space (v1:cognition:utterance rows, ordered oldest -> newest).
//
// This is a viewer only; sending utterances requires a participant
// row, which is owned by the BFF-side joinSpace flow. The cockpit
// is an operations console -- read-only chat is the right scope.
//
// Single-chat model: there is one utterance stream per space,
// visible to every participant. There is no per-user private
// thread.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/voice"

	"github.com/znasllc-io/memql-cockpit/cli/audio"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which side has keyboard focus.
type FocusPane int

const (
	FocusSpaces     FocusPane = 0
	FocusUtterances FocusPane = 1
)

// maxUtterances bounds how many rows we keep in memory per refresh.
// The pane scrolls within this window; older messages drop off.
const maxUtterances = 200

// View is the Chat tab. Mutated from both the event-loop goroutine
// (HandleEvent) and the background refresher (refreshLoop).
type View struct {
	mu    sync.RWMutex
	Theme ui.Theme

	// QueryClient returns the client bound to the active cluster.
	// Returns nil before a cluster is connected; Draw renders a
	// gated placeholder in that case.
	QueryClient func() *client.QueryClient

	// Dispatcher returns the active cluster's stream dispatcher --
	// used by the PTT flow to drive memql-sdk-go's voice.PushToTalk.
	Dispatcher func() *client.Dispatcher

	// OnRedraw is posted by the background refresher when new data
	// has landed so the event loop redraws even if no key was pressed.
	// Without this, the 3s tick mutates v.spaces / v.utterances but
	// the screen waits for the next keystroke to repaint -- the user
	// sees stale data until they interact.
	OnRedraw func()

	// PTT state, surfaced in the chat-pane title strip.
	pttActive     bool
	pttPartial    string
	pttFinal      string
	pttStatus     string // "" | "listening" | "transcribing" | "done" | "error: <msg>"
	pttCancel     context.CancelFunc
	pttStopAudio  func() error

	spaces           []spaceRow
	spaceSelected    int
	spaceScrollY     int

	utterances       []utteranceRow
	utteranceScrollY int

	Focus FocusPane

	lastFetchErr string
}

type spaceRow struct {
	ID          string
	Name        string
	OwnerUserId string
	Status      string
}

type utteranceRow struct {
	ID              string
	SpeakerID       string
	SpeakerKind     string
	SpeakerName     string
	Text            string
	CreatedAtMillis int64
}

// NewView constructs the Chat view.
func NewView(theme ui.Theme) *View {
	return &View{Theme: theme}
}

// StartRefreshLoop runs a background ticker that re-pulls spaces +
// utterances every interval.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	go func() {
		defer t.Stop()
		v.refresh()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				v.refresh()
				if v.OnRedraw != nil {
					v.OnRedraw()
				}
			}
		}
	}()
}

// Refresh is the explicit refresh entry point for callers.
func (v *View) Refresh() { v.refresh() }

func (v *View) refresh() {
	v.refreshSpaces()
	v.refreshUtterances()
}

func (v *View) refreshSpaces() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := qc.Execute(ctx, `concept==v1:cognition:space; payload.status=="active"`)
	if err != nil {
		v.mu.Lock()
		v.lastFetchErr = err.Error()
		v.mu.Unlock()
		return
	}

	rows := normalizeRows(res)
	out := make([]spaceRow, 0, len(rows))
	for _, r := range rows {
		payload, _ := r["payload"].(map[string]any)
		id, _ := r["id"].(string)
		if id == "" {
			continue
		}
		name, _ := payload["name"].(string)
		owner, _ := payload["ownerUserId"].(string)
		status, _ := payload["status"].(string)
		out = append(out, spaceRow{ID: id, Name: name, OwnerUserId: owner, Status: status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	v.mu.Lock()
	v.spaces = out
	if v.spaceSelected >= len(v.spaces) {
		v.spaceSelected = 0
	}
	v.lastFetchErr = ""
	v.mu.Unlock()
}

func (v *View) refreshUtterances() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	v.mu.RLock()
	spaceID := ""
	if v.spaceSelected < len(v.spaces) {
		spaceID = v.spaces[v.spaceSelected].ID
	}
	v.mu.RUnlock()
	if spaceID == "" {
		v.mu.Lock()
		v.utterances = nil
		v.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	q := fmt.Sprintf(`concept==v1:cognition:utterance; payload.spaceId==%q`, spaceID)
	res, err := qc.Execute(ctx, q)
	if err != nil {
		v.mu.Lock()
		v.lastFetchErr = err.Error()
		v.mu.Unlock()
		return
	}

	rows := normalizeRows(res)
	if len(rows) > maxUtterances {
		rows = rows[len(rows)-maxUtterances:]
	}
	out := make([]utteranceRow, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		payload, _ := r["payload"].(map[string]any)
		text, _ := payload["text"].(string)
		speaker, _ := payload["participantId"].(string)
		speakerKind, _ := payload["participantType"].(string)
		createdMillis := parseMillis(r["createdAt"])
		out = append(out, utteranceRow{
			ID:              id,
			SpeakerID:       speaker,
			SpeakerKind:     speakerKind,
			Text:            text,
			CreatedAtMillis: createdMillis,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAtMillis < out[j].CreatedAtMillis })

	v.mu.Lock()
	v.utterances = out
	v.utteranceScrollY = 0
	v.mu.Unlock()
}

// Draw renders the chat tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	subtleStyle := tcell.StyleDefault.Foreground(v.Theme.Subtle)
	accentStyle := tcell.StyleDefault.Foreground(v.Theme.Accent)

	if v.QueryClient == nil || v.QueryClient() == nil {
		screen.DrawText(bounds.X+2, bounds.Y+2, bounds.Width-4, "Chat: connect to a cluster from the Clusters tab to view spaces.", subtleStyle)
		return
	}

	leftW := 32
	if bounds.Width < 80 {
		leftW = bounds.Width / 3
	}
	left := ui.Rect{X: bounds.X, Y: bounds.Y, Width: leftW, Height: bounds.Height}
	right := ui.Rect{X: bounds.X + leftW, Y: bounds.Y, Width: bounds.Width - leftW, Height: bounds.Height}

	screen.DrawBox(left.X, left.Y, left.Width, left.Height, subtleStyle)
	screen.DrawText(left.X+2, left.Y, left.Width-4, fmt.Sprintf(" SPACES (%d) ", len(v.spaces)), accentStyle)

	listH := left.Height - 3
	for i, s := range v.spaces[v.spaceScrollY:] {
		row := v.spaceScrollY + i
		if i >= listH {
			break
		}
		marker := "  "
		style := subtleStyle
		if row == v.spaceSelected {
			marker = "* "
			style = accentStyle
		}
		label := s.Name
		if label == "" {
			label = s.ID
		}
		screen.DrawText(left.X+1, left.Y+1+i, left.Width-2, marker+truncate(label, left.Width-4), style)
	}
	ui.DrawBottom(screen, left, subtleStyle, 1, "Up/Dn:Move  Tab:Right")

	screen.DrawBox(right.X, right.Y, right.Width, right.Height, subtleStyle)
	title := " UTTERANCES "
	if v.spaceSelected < len(v.spaces) {
		title = fmt.Sprintf(" CHAT: %s (%d) ", truncate(displayName(v.spaces[v.spaceSelected]), 40), len(v.utterances))
	}
	screen.DrawText(right.X+2, right.Y, right.Width-4, title, accentStyle)

	contentH := right.Height - 3
	// Reserve a row for the PTT status strip when a session is active
	// or has just produced a final transcript / error to surface.
	reservePTT := v.pttActive || v.pttStatus != ""
	if reservePTT {
		contentH--
	}
	start := v.utteranceScrollY
	if start < 0 {
		start = 0
	}
	for i, u := range v.utterances[start:] {
		if i >= contentH {
			break
		}
		ts := time.UnixMilli(u.CreatedAtMillis).Format("15:04:05")
		speaker := u.SpeakerID
		if u.SpeakerKind != "" {
			speaker = fmt.Sprintf("%s[%s]", u.SpeakerID, u.SpeakerKind)
		}
		line := fmt.Sprintf("%s %s: %s", ts, speaker, u.Text)
		style := subtleStyle
		if u.SpeakerKind == "agent" || u.SpeakerKind == "si" {
			style = accentStyle
		}
		screen.DrawText(right.X+1, right.Y+1+i, right.Width-2, truncate(line, right.Width-2), style)
	}

	if reservePTT {
		pttRow := right.Y + right.Height - 2
		pttText := v.pttStatusLine()
		screen.DrawText(right.X+1, pttRow, right.Width-2, pttText, accentStyle)
	}

	hint := "Up/Dn:Scroll  v:PTT  Tab:Left"
	if v.lastFetchErr != "" {
		hint = "err: " + truncate(v.lastFetchErr, right.Width-10)
	}
	ui.DrawBottom(screen, right, subtleStyle, 1, hint)
}

// pttStatusLine renders the PTT state strip just above the chrome:
// "[mic] listening: <partial>" while active, "[mic] transcribing..."
// after release, "[mic] done: <final>" once the SDK resolves, or
// the error string otherwise. Caller must hold the read lock.
func (v *View) pttStatusLine() string {
	switch v.pttStatus {
	case "listening":
		if v.pttPartial != "" {
			return "[mic] " + truncate(v.pttPartial, 240)
		}
		return "[mic] listening... (press v again to stop)"
	case "transcribing":
		if v.pttPartial != "" {
			return "[mic] transcribing... " + truncate(v.pttPartial, 200)
		}
		return "[mic] transcribing..."
	case "done":
		return "[mic] " + truncate(v.pttFinal, 240)
	default:
		// "error: ..." or empty
		return v.pttStatus
	}
}

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	kev, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// `v` toggles push-to-talk capture: first press starts the
	// session, second press ends it cleanly so the SDK can finalize
	// the transcript. Handled OUTSIDE the locked switch below because
	// startPTT / stopPTT need to manage their own locking and spawn
	// goroutines.
	if kev.Key() == tcell.KeyRune && (kev.Rune() == 'v' || kev.Rune() == 'V') {
		v.togglePTT()
		return true
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	switch kev.Key() {
	case tcell.KeyTab:
		if v.Focus == FocusSpaces {
			v.Focus = FocusUtterances
		} else {
			v.Focus = FocusSpaces
		}
		return true
	case tcell.KeyUp:
		if v.Focus == FocusSpaces {
			if v.spaceSelected > 0 {
				v.spaceSelected--
				go v.refreshUtterances()
			}
		} else if v.utteranceScrollY > 0 {
			v.utteranceScrollY--
		}
		return true
	case tcell.KeyDown:
		if v.Focus == FocusSpaces {
			if v.spaceSelected < len(v.spaces)-1 {
				v.spaceSelected++
				go v.refreshUtterances()
			}
		} else if v.utteranceScrollY < len(v.utterances)-1 {
			v.utteranceScrollY++
		}
		return true
	}
	return false
}

// togglePTT starts a PTT session if none is active, or signals the
// active session to end. Audio capture + SDK transcription run in a
// background goroutine; partials + final land on v.pttPartial /
// v.pttFinal under v.mu, with OnRedraw posted so the title strip
// repaints in real time.
func (v *View) togglePTT() {
	v.mu.Lock()
	if v.pttActive {
		// Second press: ask the audio reader to close, which lets
		// PushToTalk see EOF on its read and send End{cancel:false}.
		if v.pttStopAudio != nil {
			_ = v.pttStopAudio()
		}
		v.pttStatus = "transcribing"
		v.mu.Unlock()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
		return
	}

	if v.Dispatcher == nil || v.Dispatcher() == nil {
		v.pttStatus = "error: no cluster connected"
		v.mu.Unlock()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
		return
	}
	dispatcher := v.Dispatcher()

	reader, stopAudio, err := audio.StartCapture(audio.DefaultFormat())
	if err != nil {
		v.pttStatus = "error: " + err.Error()
		v.mu.Unlock()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	v.pttActive = true
	v.pttPartial = ""
	v.pttFinal = ""
	v.pttStatus = "listening"
	v.pttCancel = cancel
	v.pttStopAudio = stopAudio
	v.mu.Unlock()
	if v.OnRedraw != nil {
		v.OnRedraw()
	}

	go func() {
		defer func() {
			_ = stopAudio()
			v.mu.Lock()
			v.pttActive = false
			v.pttCancel = nil
			v.pttStopAudio = nil
			v.mu.Unlock()
			if v.OnRedraw != nil {
				v.OnRedraw()
			}
		}()

		final, err := voice.PushToTalk(ctx, dispatcher, reader, voice.Options{
			Audio: voice.AudioFormat{
				Encoding:   "pcm16",
				SampleRate: 16000,
				Channels:   1,
			},
			ChunkBytes: 3200, // 100ms of 16kHz mono PCM16
			OnPartial: func(p voice.PartialTranscript) {
				v.mu.Lock()
				v.pttPartial = p.Text
				v.mu.Unlock()
				if v.OnRedraw != nil {
					v.OnRedraw()
				}
			},
		})
		v.mu.Lock()
		if err != nil {
			v.pttStatus = "error: " + err.Error()
		} else if final != nil {
			v.pttFinal = final.Text
			v.pttStatus = "done"
		}
		v.mu.Unlock()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
	}()
}

// normalizeRows accepts the protojson-decoded Execute result and
// returns it as a flat []map[string]any of row records.
func normalizeRows(res any) []map[string]any {
	if res == nil {
		return nil
	}
	switch x := res.(type) {
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		// Engine may wrap arrays in {"rows": [...]} or similar.
		for _, key := range []string{"rows", "results", "items", "data"} {
			if arr, ok := x[key].([]any); ok {
				out := make([]map[string]any, 0, len(arr))
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		// Single-row case.
		return []map[string]any{x}
	}
	return nil
}

func parseMillis(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		// Try RFC3339 first.
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UnixMilli()
		}
		// Try JSON-number-as-string.
		var n int64
		if _, err := fmt.Sscan(x, &n); err == nil {
			return n
		}
	}
	return 0
}

func displayName(s spaceRow) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// hashCheck guards against unused json import; we keep json available
// in case future iterations want to inline a payload pretty-printer.
var _ = json.Valid
var _ = strings.TrimSpace
