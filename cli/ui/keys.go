package ui

import "github.com/gdamore/tcell/v2"

// KeyBinding maps a key event to a handler function.
type KeyBinding struct {
	Key     tcell.Key
	Rune    rune // for rune-based keys (e.g., Alt+1)
	Mod     tcell.ModMask
	Handler func()
}

// KeyMap manages a stack of key bindings with priority.
type KeyMap struct {
	global []KeyBinding
}

// NewKeyMap creates an empty key map.
func NewKeyMap() *KeyMap {
	return &KeyMap{}
}

// Bind registers a global key binding.
func (km *KeyMap) Bind(key tcell.Key, mod tcell.ModMask, handler func()) {
	km.global = append(km.global, KeyBinding{Key: key, Mod: mod, Handler: handler})
}

// BindRune registers a rune-based global key binding (e.g., Alt+q).
func (km *KeyMap) BindRune(r rune, mod tcell.ModMask, handler func()) {
	km.global = append(km.global, KeyBinding{Key: tcell.KeyRune, Rune: r, Mod: mod, Handler: handler})
}

// Handle dispatches a key event to the matching binding.
// Returns true if a binding was triggered.
func (km *KeyMap) Handle(ev *tcell.EventKey) bool {
	for _, b := range km.global {
		if b.Key == ev.Key() && b.Mod == ev.Modifiers() {
			if b.Key == tcell.KeyRune && b.Rune != ev.Rune() {
				continue
			}
			b.Handler()
			return true
		}
	}
	return false
}
