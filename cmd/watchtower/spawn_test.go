package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/repocfg"
)

func TestDialExistingDaemonUsesOnlyExistingSocket(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	base, repo := t.TempDir(), t.TempDir()
	dataDir := repocfg.RepoDataDir(base, repo)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dataDir, sockFileName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dialExistingDaemon(base, repo)
	if err != nil {
		t.Fatalf("dialExistingDaemon: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	listener.Close()
	if _, err := dialExistingDaemon(base, repo); err == nil {
		t.Fatal("missing daemon socket unexpectedly connected")
	}
	for _, name := range []string{pidFileName, logFileName} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Fatalf("dial-only helper created %s: %v", name, err)
		}
	}
}
