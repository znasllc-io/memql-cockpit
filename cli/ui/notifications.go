package ui

import (
	"fmt"
	"sync"

	"github.com/gdamore/tcell/v2"
)

// Severity categorizes a notification for coloring + icon selection.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Notification is one entry in the notifications center. Each entry is
// keyed by ID -- re-syncing with the same ID updates the message
// in-place instead of producing duplicates. The intended use is a
// "source-of-truth" model: callers Sync the current state on every
// redraw and the widget reconciles against its own list.
type Notification struct {
	ID       string
	Severity Severity
	Message  string
	// NoCopy, when true, hides the "Ctrl+Y:Copy" hint and disables the
	// Ctrl+Y key binding for this notification. Used for meta-acks
	// that describe UI actions (e.g. the "Message copied to
	// clipboard." toast after a successful copy) -- copying that text
	// back to the clipboard makes no sense.
	NoCopy bool
}

// Notifications is the center-of-header widget that stores a short
// FIFO of messages. Thread-safe: callers (the app's draw loop,
// lifecycle goroutines) can Sync from any goroutine; the renderer
// reads under the same lock.
//
// Dismiss-suppression: when the user dismisses a notification, the
// widget remembers the exact message it was showing. A subsequent
// Sync with the same ID + same message is ignored, so the dismissed
// entry doesn't keep popping back. If Sync provides a DIFFERENT
// message for the same ID (e.g. retry attempt 1 -> attempt 2, or
// retrying -> unreachable), the suppression is lifted and the new
// message surfaces.
type Notifications struct {
	mu        sync.Mutex
	list      []Notification
	dismissed map[string]string // id -> last-dismissed message

	// OnChange fires (without the lock held) whenever the visible set
	// changes, so the app can schedule a redraw. Optional.
	OnChange func()
}

// NewNotifications returns an empty notifications center.
func NewNotifications() *Notifications {
	return &Notifications{dismissed: make(map[string]string)}
}

// Sync upserts a notification. Passing an empty message removes any
// existing entry for id (equivalent to Clear). Honors the user's
// dismiss choice: if the identical message was dismissed, it stays
// hidden until the caller surfaces a different message under that id.
func (n *Notifications) Sync(id string, severity Severity, message string) {
	n.sync(id, severity, message, false)
}

// SyncMeta is like Sync but flags the notification as a UI-action ack
// ("Message copied to clipboard.", "Settings saved.", etc.). The
// header renders these without the Ctrl+Y:Copy hint, and the Ctrl+Y
// key no-ops on them -- copying a copy-ack back to the clipboard is
// nonsense.
func (n *Notifications) SyncMeta(id string, severity Severity, message string) {
	n.sync(id, severity, message, true)
}

func (n *Notifications) sync(id string, severity Severity, message string, noCopy bool) {
	n.mu.Lock()
	changed := false
	if message == "" {
		// Remove any existing entry for id.
		for i := range n.list {
			if n.list[i].ID == id {
				n.list = append(n.list[:i], n.list[i+1:]...)
				changed = true
				break
			}
		}
		// Clearing also clears the dismiss memory -- a future message
		// under the same id starts fresh.
		delete(n.dismissed, id)
	} else if n.dismissed[id] == message {
		// User already dismissed this exact message. Don't re-add.
	} else {
		// Dismiss suppression only applies to the exact dismissed
		// message, so a state change lifts it.
		if n.dismissed[id] != "" && n.dismissed[id] != message {
			delete(n.dismissed, id)
		}
		found := false
		for i := range n.list {
			if n.list[i].ID == id {
				if n.list[i].Message != message || n.list[i].Severity != severity || n.list[i].NoCopy != noCopy {
					n.list[i].Severity = severity
					n.list[i].Message = message
					n.list[i].NoCopy = noCopy
					changed = true
				}
				found = true
				break
			}
		}
		if !found {
			n.list = append(n.list, Notification{ID: id, Severity: severity, Message: message, NoCopy: noCopy})
			changed = true
		}
	}
	cb := n.OnChange
	n.mu.Unlock()
	if changed && cb != nil {
		cb()
	}
}

// DismissCurrent removes the front notification and remembers the
// dismissed message so a follow-up Sync with the same content stays
// hidden. No-op when the list is empty.
func (n *Notifications) DismissCurrent() {
	n.mu.Lock()
	if len(n.list) == 0 {
		n.mu.Unlock()
		return
	}
	front := n.list[0]
	n.dismissed[front.ID] = front.Message
	n.list = n.list[1:]
	cb := n.OnChange
	n.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Clear forcibly removes a notification and forgets any dismiss
// suppression under that id. Used when the underlying condition goes
// away completely (e.g. a cluster reaches Connected).
func (n *Notifications) Clear(id string) {
	n.Sync(id, "", "")
}

// Current returns the front notification, or ok=false if the list is
// empty. Safe to call from the draw loop.
func (n *Notifications) Current() (Notification, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.list) == 0 {
		return Notification{}, false
	}
	return n.list[0], true
}

// Count returns the total pending notifications (including the front).
func (n *Notifications) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.list)
}

// Render paints the current notification into the given horizontal
// slot [x, x+maxW) on row y. Callers size the slot to avoid colliding
// with the title on the left or the nav hint on the right. Truncates
// the message when space is tight -- the severity icon and "[+N]"
// counter always get priority over the message text.
func (n *Notifications) Render(screen *Screen, theme Theme, x, y, maxW int) {
	note, ok := n.Current()
	if !ok || maxW <= 0 {
		return
	}
	count := n.Count()

	barBG := tcell.NewRGBColor(18, 18, 22)
	icon, color := severityStyle(note.Severity, theme)
	iconStyle := tcell.StyleDefault.Foreground(color).Background(barBG).Bold(true)
	msgStyle := tcell.StyleDefault.Foreground(theme.FG).Background(barBG)
	dimStyle := tcell.StyleDefault.Foreground(theme.Subtle).Background(barBG)

	counter := ""
	if count > 1 {
		counter = fmt.Sprintf(" [+%d]", count-1)
	}
	// Minimum reserved glyphs: icon + dismiss/copy hints + counter.
	// If we can't fit at least the icon + 4 message chars, skip
	// rendering so we don't draw garbage. The hint collapses from
	// "Ctrl+Y:Copy Ctrl+K:Dismiss" down to just "Ctrl+K:Dismiss" and
	// finally to "" as horizontal space shrinks.
	//
	// Meta notifications (NoCopy, e.g. the "Message copied to
	// clipboard." ack) skip the Copy hint entirely -- copying a copy
	// ack is nonsense -- and go straight to the short form.
	fullHint := "  Ctrl+Y:Copy  Ctrl+K:Dismiss"
	shortHint := "  Ctrl+K:Dismiss"
	dismissHint := fullHint
	if note.NoCopy {
		dismissHint = shortHint
	}
	if maxW < 6+len(counter)+len(dismissHint) {
		dismissHint = shortHint
	}
	if maxW < 6+len(counter)+len(dismissHint) {
		dismissHint = ""
	}
	if maxW < 6 {
		return
	}

	col := x
	screen.SetCell(col, y, []rune(icon)[0], iconStyle)
	col += 2 // icon + space

	// How much width is left for the message body?
	budget := maxW - (col - x) - len(counter) - len(dismissHint)
	if budget < 1 {
		budget = 1
	}
	msg := note.Message
	if len(msg) > budget {
		if budget > 1 {
			msg = msg[:budget-1] + "…"
		} else {
			msg = "…"
		}
	}
	screen.DrawText(col, y, budget, msg, msgStyle)
	col += len(msg)
	if counter != "" {
		screen.DrawText(col, y, len(counter), counter, dimStyle)
		col += len(counter)
	}
	if dismissHint != "" {
		screen.DrawText(col, y, len(dismissHint), dismissHint, dimStyle)
	}
}

// severityStyle returns the glyph + color to use for a given severity.
// Icons are 1-rune so cell math stays predictable.
func severityStyle(s Severity, theme Theme) (string, tcell.Color) {
	switch s {
	case SeverityError:
		return "●", theme.Error
	case SeverityWarning:
		return "◆", theme.Warning
	case SeverityInfo:
		return "○", theme.Info
	default:
		return "○", theme.Subtle
	}
}
