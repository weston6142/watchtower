package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wbushyeager/guildhall/internal/proto"
	"github.com/wbushyeager/guildhall/internal/repocfg"
)

// Well-known filenames inside a repo's data dir.
const (
	sockFileName = "guildhall.sock"
	pidFileName  = "daemon.pid"
	logFileName  = "daemon.log"
)

const (
	daemonStartTimeout = 5 * time.Second
	daemonPollInterval = 100 * time.Millisecond
	daemonLogTailLines = 10
)

// resolveRepo returns repoFlag if set, otherwise walks up from CWD to the
// nearest .guildhall directory. Exits the process on failure.
func resolveRepo(repoFlag string) string {
	if repoFlag != "" {
		return repoFlag
	}
	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	repo, err := repocfg.FindRepo(cwd)
	if err != nil {
		fatal(err)
	}
	return repo
}

// mustDial resolves the target repo, connects to its daemon socket, and
// spawns a detached daemon first if none is listening.
func mustDial(base, repoFlag string) *proto.Client {
	repo := resolveRepo(repoFlag)
	dataDir := repocfg.RepoDataDir(base, repo)
	sock := filepath.Join(dataDir, sockFileName)
	if c, err := proto.Dial(sock); err == nil {
		return c
	}
	clearStaleSocket(dataDir, sock)
	if err := spawnDaemon(base, repo, dataDir); err != nil {
		fatal(fmt.Errorf("starting daemon: %w", err))
	}
	deadline := time.Now().Add(daemonStartTimeout)
	for time.Now().Before(deadline) {
		if c, err := proto.Dial(sock); err == nil {
			fmt.Fprintln(os.Stderr, "started daemon for", repo)
			return c
		}
		time.Sleep(daemonPollInterval)
	}
	fatal(fmt.Errorf("daemon did not come up within %s; last log lines:\n%s",
		daemonStartTimeout, tailFile(filepath.Join(dataDir, logFileName), daemonLogTailLines)))
	return nil
}

// clearStaleSocket removes a socket left behind by a dead daemon. If the
// pidfile's process is still alive we leave the socket alone (the daemon
// may just be starting up or wedged — the connect retry loop handles it).
func clearStaleSocket(dataDir, sock string) {
	if _, err := os.Stat(sock); err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(dataDir, pidFileName))
	if err == nil {
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr == nil {
			if proc, ferr := os.FindProcess(pid); ferr == nil && proc.Signal(syscall.Signal(0)) == nil {
				return // process alive
			}
		}
	}
	os.Remove(sock)
}

func spawnDaemon(base, repo, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dataDir, logFileName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon", "--data", base, "--repo", repo)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive client exit
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
