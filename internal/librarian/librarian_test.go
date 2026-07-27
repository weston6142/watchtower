package librarian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextConcatenatesMemory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b-arch.md"), []byte("services: pay, cart"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-conventions.md"), []byte("tabs not spaces"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Librarian{MemoryDir: dir}
	got, err := l.Context()
	if err != nil {
		t.Fatal(err)
	}
	ia, ib := strings.Index(got, "a-conventions.md"), strings.Index(got, "b-arch.md")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("order/content wrong:\n%s", got)
	}
	if strings.Contains(got, "ignore.txt") {
		t.Fatal("non-md file leaked")
	}
}

func TestContextMissingDirIsEmpty(t *testing.T) {
	l := &Librarian{MemoryDir: "/nonexistent/xyz"}
	got, err := l.Context()
	if err != nil || got != "" {
		t.Fatalf("want empty, got %q err %v", got, err)
	}
}
