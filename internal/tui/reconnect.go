package tui

import "github.com/weston6142/watchtower/internal/proto"

// Session is one serialized connection to the repository daemon.
type Session interface {
	Do(proto.Command) (proto.Response, error)
	Close() error
}

// Dialer creates a replacement session without owning daemon startup.
type Dialer func() (Session, error)
