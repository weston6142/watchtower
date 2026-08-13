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

type systemClock uint8

func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

const SystemClock systemClock = 0
