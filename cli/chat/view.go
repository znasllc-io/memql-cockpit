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
//
// Migrated to the cli/ui widget layer (epic #81). The spaces list
// uses ui.ListPane; the utterance pane stays hand-rolled because
// DetailPane can't express the per-row subtle/accent mix (humans
// vs agent/si) without forcing bold on agent rows. A follow-up can
// extend DetailPane for chat-style auto-pinned mixed-style logs.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
const maxUtterances = 200

// pttSilenceWindow is how long without a new partial transcript
// counts as "user stopped talking" and triggers an auto-stop on the
// recording. Long enough to ride out a thinking pause between
// sentences (4s), short enough that a forgotten Ctrl+Space doesn't
// leave the mic open indefinitely.
const pttSilenceWindow = 4 * time.Second

// pttWatchdogTick is how often the silence watchdog re-checks
// time-since-last-partial. Smaller = snappier release, more CPU.
const pttWatchdogTick = 500 * time.Millisecond

// View is the Chat tab. Mutated from both the event-loop goroutine
// (HandleEvent) and the background refresher (refreshLoop).
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	// QueryClient returns the client bound to the active cluster.
	// Returns nil before a cluster is connected; Draw renders a
	// gated placeholder in that case.
	QueryClient func() *client.QueryClient

	// Dispatcher returns the active cluster's stream dispatcher --
	// used by the PTT flow to drive memql-sdk-go's voice.PushToTalk.
	Dispatcher func() *client.Dispatcher

	// PTT state, surfaced in the chat-pane title strip.
	pttActive        bool
	pttPartial       string
	pttFinal         string
	pttStatus        string // "" | "listening" | "transcribing" | "done" | "error: <msg>"
	pttCancel        context.CancelFunc
	pttStopAudio     func() error
	pttLastPartialAt time.Time // tracked by OnPartial; drives silence auto-stop

	spaces []spaceRow
	// userPickedSpace tracks whether the user has manually selected a
	// space this session. While false, the refresher auto-snaps the
	// highlight to today's daily on every refresh -- so a freshly
	// provisioned daily lands selected even when it arrives a tick
	// or two after the first paint. Flipped true the first time
	// HandleEvent moves the highlight.
	userPickedSpace bool

	utterances       []utteranceRow
	utteranceScrollY int

	Focus FocusPane

	// ensureRan flips true after the first successful
	// ensureDailySpaceForCaller call. Cheap idempotent capability,
	// but no point hammering it.
	ensureRan bool

	// spaceList renders the left pane. Selected / ScrollY live in
	// the widget; the view drives Count + Focused per render.
	spaceList ui.ListPane
}

type spaceRow struct {
	ID          string
	Name        string
	OwnerUserId string
	Status      string
	Kind        string
}

type utteranceRow struct {
	ID              string
	SpeakerID       string
	SpeakerKind     string
	SpeakerName     string
	Text            string
	CreatedAtMillis int64
}

// NewView constructs the Chat view. The spaces list uses
// RowsPerItem=2 to match the planner's plan-list visual: primary
// line carries name + Today: prefix, dimmed subtitle carries
// kind + owner.
func NewView(theme ui.Theme) *View {
	v := &View{}
	v.Theme = theme
	v.spaceList.RowsPerItem = 2
	v.spaceList.Render = v.renderSpaceRow
	return v
}

// StartRefreshLoop runs a background ticker that re-pulls spaces +
// utterances every interval. Wraps BaseView's context-based helper
// so the legacy stop-channel API stays stable for app.go.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, v.refresh)
}

// Refresh is the explicit refresh entry point for callers.
func (v *View) Refresh() { v.refresh() }

func (v *View) refresh() {
	v.ensureDailyOnce()
	v.refreshSpaces()
	v.refreshUtterances()
}

// ensureDailyOnce fires integration.dailyspace.ensureForCaller exactly
// once per session, on the first refresh after a cluster connects.
func (v *View) ensureDailyOnce() {
	v.Mu.RLock()
	already := v.ensureRan
	v.Mu.RUnlock()
	if already {
		return
	}
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	v.Mu.Lock()
	v.ensureRan = true
	v.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := qc.LogicEnsureDailySpaceForCaller(ctx, client.LogicEnsureDailySpaceForCallerArgs{}); err != nil {
		// Push the failure through the header notifications center
		// (with copy + dismiss) instead of the chrome strip. Clear
		// the latch so the next 3s tick retries; a transient failure
		// shouldn't strand the user without a daily.
		if v.OnStatus != nil {
			v.OnStatus("ensure daily space: " + err.Error())
		}
		v.Mu.Lock()
		v.ensureRan = false
		v.Mu.Unlock()
	}
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

	// Typed primitive -- the engine returns the user's active spaces
	// already filtered by per-row authz, projected through the
	// `spaceFull` shape. Fields land at the row top level.
	res, err := qc.QueryActiveSpaces(ctx, client.QueryActiveSpacesArgs{})
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus("load spaces: " + err.Error())
		}
		return
	}

	rows := res.Rows()
	out := make([]spaceRow, 0, len(rows))
	for _, r := range rows {
		id := client.RowString(r, "id")
		if id == "" {
			continue
		}
		out = append(out, spaceRow{
			ID:          id,
			Name:        client.RowString(r, "name"),
			OwnerUserId: client.RowString(r, "ownerUserId"),
			Status:      client.RowString(r, "status"),
			Kind:        client.RowString(r, "kind"),
		})
	}
	// Pin daily spaces at the top.
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := out[i].Kind == "daily", out[j].Kind == "daily"
		if di != dj {
			return di
		}
		return out[i].ID < out[j].ID
	})

	v.Mu.Lock()
	v.spaces = out
	v.spaceList.Count = len(out)
	if v.spaceList.Selected >= len(v.spaces) {
		v.spaceList.Selected = 0
	}
	// Until the user has moved the highlight themselves, anchor the
	// selection on today's daily.
	if !v.userPickedSpace && len(v.spaces) > 0 {
		idx := indexOfDaily(v.spaces)
		if idx < 0 {
			idx = 0
		}
		if idx != v.spaceList.Selected {
			v.spaceList.Selected = idx
			v.spaceList.ScrollY = 0
		}
	}
	v.Mu.Unlock()
}

// indexOfDaily returns the index of the first daily-kind row, or -1.
func indexOfDaily(rows []spaceRow) int {
	for i, r := range rows {
		if r.Kind == "daily" {
			return i
		}
	}
	return -1
}

func (v *View) refreshUtterances() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	v.Mu.RLock()
	spaceID := ""
	if v.spaceList.Selected < len(v.spaces) {
		spaceID = v.spaces[v.spaceList.Selected].ID
	}
	v.Mu.RUnlock()
	if spaceID == "" {
		v.Mu.Lock()
		v.utterances = nil
		v.Mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := qc.QuerySpaceUtterances(ctx, client.QuerySpaceUtterancesArgs{SpaceId: spaceID})
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus("load utterances: " + err.Error())
		}
		return
	}

	rows := res.Rows()
	if len(rows) > maxUtterances {
		rows = rows[len(rows)-maxUtterances:]
	}
	out := make([]utteranceRow, 0, len(rows))
	for _, r := range rows {
		id := client.RowString(r, "id")
		createdMillis := parseMillis(r["createdAt"])
		out = append(out, utteranceRow{
			ID:              id,
			SpeakerID:       client.RowString(r, "participantId"),
			SpeakerKind:     client.RowString(r, "participantType"),
			Text:            client.RowString(r, "text"),
			CreatedAtMillis: createdMillis,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAtMillis < out[j].CreatedAtMillis })

	v.Mu.Lock()
	v.utterances = out
	v.utteranceScrollY = 0
	v.Mu.Unlock()
}

// Draw renders the chat tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

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

	// Left pane: spaces list inside a bordered box. Pass bounds
	// shifted one column inward so PaneTitle's content lands AFTER
	// the box's left ┌ corner instead of overdrawing it.
	screen.DrawBox(left.X, left.Y, left.Width, left.Height, subtleStyle)
	ui.PaneTitle{
		Title:   "SPACES",
		Counter: ui.FormatCount(len(v.spaces)),
		Focused: v.Focus == FocusSpaces,
	}.Draw(screen, ui.Rect{X: left.X + 1, Y: left.Y, Width: left.Width - 2, Height: left.Height}, v.Theme)
	v.spaceList.Count = len(v.spaces)
	v.spaceList.Focused = v.Focus == FocusSpaces
	// ListPane bounds: inside the border, above the chrome row.
	listInner := ui.Rect{
		X:      left.X + 1,
		Y:      left.Y + 1,
		Width:  left.Width - 2,
		Height: left.Height - 3, // border top + border bottom + chrome row
	}
	if listInner.Height < 1 {
		listInner.Height = 1
	}
	v.spaceList.Draw(screen, listInner, v.Theme)
	ui.DrawBottom(screen, left, subtleStyle, 1, hintsForSpaces())

	// Right pane: utterance scroll inside a bordered box. Stays
	// hand-rolled -- DetailPane can't express the per-row subtle
	// (human) vs. accent (agent/si) mix without forcing bold on
	// agent rows. A follow-up can extend DetailPane for chat-style
	// auto-pinned mixed-style logs.
	screen.DrawBox(right.X, right.Y, right.Width, right.Height, subtleStyle)
	// CHAT title is constant -- the embedded space name in the
	// previous " CHAT: <spaceName> (N) " form was redundant with the
	// highlighted row in the left pane (chrome contract: no
	// embedded ids/names in titles). Counter is a bare utterance
	// count for the selected space.
	ui.PaneTitle{
		Title:   "CHAT",
		Counter: ui.FormatCount(len(v.utterances)),
		Focused: v.Focus == FocusUtterances,
	}.Draw(screen, ui.Rect{X: right.X + 1, Y: right.Y, Width: right.Width - 2, Height: right.Height}, v.Theme)

	contentH := right.Height - 3
	// Reserve a row for the PTT status strip when a session is
	// active or has just produced a final transcript / error.
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

	ui.DrawBottom(screen, right, subtleStyle, 1, hintsForUtterances())
}

// renderSpaceRow paints a 2-row space-list entry. Primary line:
// marker (`* ` selected / `  ` else) + label, with `Today: ` prefix
// for daily-kind spaces so the user can tell their daily apart from
// any other space with the same name. Subtitle: kind + owner in the
// dimmed planner-style metadata slot.
func (v *View) renderSpaceRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.spaces) {
		return
	}
	s := v.spaces[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	marker := "  "
	if sel {
		marker = "* "
	}
	label := s.Name
	if label == "" {
		label = s.ID
	}
	if s.Kind == "daily" {
		label = "Today: " + label
	}
	screen.DrawText(bounds.X, bounds.Y, bounds.Width, marker+truncate(label, bounds.Width-2), primary)

	var subStr string
	switch {
	case s.Kind != "" && s.OwnerUserId != "":
		subStr = s.Kind + "  ·  " + s.OwnerUserId
	case s.Kind != "":
		subStr = s.Kind
	case s.OwnerUserId != "":
		subStr = s.OwnerUserId
	}
	if subStr != "" {
		screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-2, subStr, sub)
	}
}

// pttStatusLine renders the PTT state strip just above the chrome.
// Caller must hold the read lock.
func (v *View) pttStatusLine() string {
	switch v.pttStatus {
	case "listening":
		if v.pttPartial != "" {
			return "[mic] " + truncate(v.pttPartial, 240)
		}
		return "[mic] listening... (Ctrl+Space again to stop)"
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

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForSpaces() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForUtterances() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "Ctrl+Space", Label: "Talk"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	kev, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Ctrl+Space toggles push-to-talk capture: first press starts a
	// recording session, second press stops it. Bare-space hold-to-
	// talk (the Claude-app pattern) doesn't work in terminals --
	// tcell 2.13.8 opts out of the Kitty keyboard protocol's key-
	// release events, so there's no "release" signal to bind. Ctrl+
	// Space avoids the literal-space-typing conflict that bare space
	// would have once an utterance composer ships. Handled OUTSIDE
	// the locked switch below because startPTT / stopPTT manage
	// their own locking and spawn goroutines.
	if kev.Key() == tcell.KeyCtrlSpace {
		v.togglePTT()
		return true
	}

	v.Mu.Lock()
	defer v.Mu.Unlock()

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
			v.spaceList.Focused = true
			prev := v.spaceList.Selected
			if v.spaceList.HandleEvent(kev) && v.spaceList.Selected != prev {
				v.userPickedSpace = true
				go v.refreshUtterances()
			}
		} else if v.utteranceScrollY > 0 {
			v.utteranceScrollY--
		}
		return true
	case tcell.KeyDown:
		if v.Focus == FocusSpaces {
			v.spaceList.Focused = true
			prev := v.spaceList.Selected
			if v.spaceList.HandleEvent(kev) && v.spaceList.Selected != prev {
				v.userPickedSpace = true
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
// v.pttFinal under v.Mu, with Redraw posted so the title strip
// repaints in real time.
func (v *View) togglePTT() {
	v.Mu.Lock()
	if v.pttActive {
		// Second press: ask the audio reader to close.
		if v.pttStopAudio != nil {
			_ = v.pttStopAudio()
		}
		v.pttStatus = "transcribing"
		v.Mu.Unlock()
		v.Redraw()
		return
	}

	if v.Dispatcher == nil || v.Dispatcher() == nil {
		v.pttStatus = "error: no cluster connected"
		v.Mu.Unlock()
		v.Redraw()
		return
	}
	dispatcher := v.Dispatcher()

	reader, stopAudio, err := audio.StartCapture(audio.DefaultFormat())
	if err != nil {
		v.pttStatus = "error: " + err.Error()
		v.Mu.Unlock()
		v.Redraw()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	v.pttActive = true
	v.pttPartial = ""
	v.pttFinal = ""
	v.pttStatus = "listening"
	v.pttCancel = cancel
	v.pttStopAudio = stopAudio
	v.pttLastPartialAt = time.Now() // start the silence window from now
	v.Mu.Unlock()
	v.Redraw()

	// Silence watchdog: stops recording if no partial transcript
	// has arrived for pttSilenceWindow. Catches a forgotten
	// Ctrl+Space and the "I'm done talking" case where the user
	// just stops speaking.
	go v.pttSilenceWatchdog(ctx)

	go func() {
		defer func() {
			_ = stopAudio()
			v.Mu.Lock()
			v.pttActive = false
			v.pttCancel = nil
			v.pttStopAudio = nil
			v.Mu.Unlock()
			v.Redraw()
		}()

		final, err := voice.PushToTalk(ctx, dispatcher, reader, voice.Options{
			Audio: voice.AudioFormat{
				Encoding:   "pcm16",
				SampleRate: 16000,
				Channels:   1,
			},
			ChunkBytes: 3200, // 100ms of 16kHz mono PCM16
			OnPartial: func(p voice.PartialTranscript) {
				v.Mu.Lock()
				v.pttPartial = p.Text
				v.pttLastPartialAt = time.Now() // refresh the watchdog
				v.Mu.Unlock()
				v.Redraw()
			},
		})
		v.Mu.Lock()
		if err != nil {
			v.pttStatus = "error: " + err.Error()
		} else if final != nil {
			v.pttFinal = final.Text
			v.pttStatus = "done"
		}
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// pttSilenceWatchdog runs while a PTT session is active. Every
// pttWatchdogTick it checks how long it's been since the last
// partial transcript; once that gap exceeds pttSilenceWindow, it
// asks the audio reader to close, which lets PushToTalk see EOF
// and finalize the transcript via the same path the manual second
// Ctrl+Space takes.
//
// Exits when ctx is canceled (manual stop already happened) or when
// the recording is no longer active (audio goroutine cleaned up
// first).
func (v *View) pttSilenceWatchdog(ctx context.Context) {
	ticker := time.NewTicker(pttWatchdogTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v.Mu.RLock()
			active := v.pttActive
			lastAt := v.pttLastPartialAt
			stopAudio := v.pttStopAudio
			status := v.pttStatus
			v.Mu.RUnlock()
			if !active {
				return
			}
			// Only trigger from "listening" -- once the user has
			// already pressed Ctrl+Space to stop (status = "transcribing")
			// the audio stream is closing on its own; don't double-trip.
			if status != "listening" {
				continue
			}
			if time.Since(lastAt) > pttSilenceWindow {
				v.Mu.Lock()
				v.pttStatus = "transcribing"
				v.Mu.Unlock()
				if stopAudio != nil {
					_ = stopAudio()
				}
				v.Redraw()
				return
			}
		}
	}
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
