package core

import "time"

// StartOfDay returns local midnight of now's calendar day, in now's location.
// Shared by the daemon's shipped-today count and the TUI's shipped shelf so the
// two cannot drift on what "today" means.
func StartOfDay(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}
