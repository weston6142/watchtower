package repocfg

import (
	"testing"
)

func TestRegisterAndList(t *testing.T) {
	base := t.TempDir()
	if err := Register(base, "/repo/alpha"); err != nil {
		t.Fatal(err)
	}
	if err := Register(base, "/repo/beta"); err != nil {
		t.Fatal(err)
	}
	if err := Register(base, "/repo/alpha"); err != nil { // idempotent
		t.Fatal(err)
	}
	entries, err := ListRegistered(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Path != "/repo/alpha" || entries[1].Path != "/repo/beta" {
		t.Fatalf("bad order: %+v", entries)
	}
	if entries[0].RegisteredAt.IsZero() {
		t.Fatal("registered_at not set")
	}
}

func TestListRegisteredEmpty(t *testing.T) {
	entries, err := ListRegistered(t.TempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("want empty no error, got %v %v", entries, err)
	}
}
