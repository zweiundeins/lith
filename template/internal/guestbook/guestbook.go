// Package guestbook is the example feature: everyone signs one shared
// guestbook, and every tab sees what became of its own last signature.
// Commands (commands.go) are the only writes; queries (queries.go) read in
// the transaction they are given.
package guestbook

import "time"

const (
	MaxName    = 40             // characters
	MaxMessage = 280            // characters
	Shown      = 20             // entries on the page
	Kept       = 1000           // entries the sweep keeps
	TabTTL     = 24 * time.Hour // how long a tab's state outlives its last signature
)

// Topic is touched by every change to the entries.
const Topic = "guestbook"

// TabTopic is touched by every change to the state of the tab.
func TabTopic(tab string) string { return "tab:" + tab }

// Entry is one signature.
type Entry struct {
	ID      int64
	Name    string
	Message string
	At      time.Time
}

// TabState is the state of one tab, a row in tab_state: the op id of its
// last signature, and why that added no entry, if it did not.
type TabState struct {
	Op      string `json:"op,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

// Accepted reports whether the tab's last signature added an entry.
func (s TabState) Accepted() bool { return s.Op != "" && s.Refusal == "" }
