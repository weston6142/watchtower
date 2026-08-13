package core

import "time"

// Clock supplies semantic workflow time. Deadline and polling clocks remain
// separate so deterministic recovery can freeze persisted timestamps safely.
type Clock interface {
	Now() time.Time
}

type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time {
	return f().UTC()
}

var SystemClock Clock = ClockFunc(time.Now)
