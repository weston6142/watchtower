package main_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
)

type towerProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	output strings.Builder
	done   chan struct{}
}

type towerOutput struct {
	process *towerProcess
}

func (w towerOutput) Write(data []byte) (int, error) {
	w.process.mu.Lock()
	defer w.process.mu.Unlock()
	return w.process.output.Write(data)
}

func startTowerProcess(t *testing.T, bin, base, repo string) *towerProcess {
	t.Helper()
	args := []string{"-qF", "/dev/stdout", "/bin/sh", "-c",
		"stty columns 120 rows 40; exec \"$@\"", "tower-pty", bin, "tower", "--data", base, "--repo", repo, "--reduced-motion"}
	if runtime.GOOS == "linux" {
		command := strings.Join([]string{
			"stty columns 120 rows 40; exec",
			shellQuote(bin), "tower", "--data", shellQuote(base), "--repo", shellQuote(repo), "--reduced-motion",
		}, " ")
		args = []string{"-qefc", command, "/dev/null"}
	}
	cmd := exec.Command("script", args...)
	cmd.Env = append(os.Environ(), "TERM=dumb", "COLUMNS=120", "LINES=40")
	cmd.Dir = repo
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	process := &towerProcess{t: t, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	cmd.Stdout = towerOutput{process: process}
	cmd.Stderr = towerOutput{process: process}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(process.close)
	return process
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (p *towerProcess) snapshot() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

func (p *towerProcess) resetOutput() {
	p.mu.Lock()
	p.output.Reset()
	p.mu.Unlock()
}

func (p *towerProcess) send(keys string) {
	p.t.Helper()
	if _, err := io.WriteString(p.stdin, keys); err != nil {
		p.t.Fatalf("tower input %q: %v", keys, err)
	}
}

func (p *towerProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *towerProcess) waitForOutput(needle string, timeout time.Duration) string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw := p.snapshot()
		plain := ansi.Strip(raw)
		if strings.Contains(plain, needle) {
			return plain
		}
		if !p.alive() {
			p.t.Fatalf("tower exited while waiting for %q:\n%s", needle, plain)
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("tower did not render %q:\n%s", needle, ansi.Strip(p.snapshot()))
	return ""
}

func (p *towerProcess) close() {
	if p.alive() {
		_, _ = io.WriteString(p.stdin, "q")
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}
	}
	_ = p.stdin.Close()
}

func daemonPID(t *testing.T, base, repo string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repocfg.RepoDataDir(base, repo), "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	state, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
}

func waitForProcessState(t *testing.T, pid int, wantAlive bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processAlive(pid) == wantAlive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d alive=%v, want %v", pid, processAlive(pid), wantAlive)
}

func waitForSocket(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if client, err := proto.Dial(path); err == nil {
			_ = client.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s did not accept connections", path)
}

func waitForSocketUnavailable(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := proto.Dial(path)
		if err != nil {
			return
		}
		_ = client.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s remained available", path)
}

func startReplacementDaemon(t *testing.T, bin, base, repo string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "daemon", "--data", base, "--repo", repo)
	cmd.Dir = repo
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForSocket(t, filepath.Join(repocfg.RepoDataDir(base, repo), "watchtower.sock"), 5*time.Second)
	t.Cleanup(func() { stopProcess(t, cmd) })
	return cmd
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if processAlive(cmd.Process.Pid) {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		waitForProcessState(t, cmd.Process.Pid, false, 5*time.Second)
	}
	_ = cmd.Wait()
}

type e2eOptions struct {
	mode string
}

type flowE2E struct {
	t      *testing.T
	bin    string
	root   string
	base   string
	repo   string
	remote string
	lanes  string
	log    string
}

func newFlowE2E(t *testing.T, options e2eOptions) *flowE2E {
	t.Helper()
	t.Setenv("TMPDIR", "/tmp")
	root, err := os.MkdirTemp("/tmp", "watchtower-flow-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &flowE2E{
		t: t, bin: buildBinary(t), root: root,
		base: filepath.Join(root, "data"), repo: filepath.Join(root, "repo"),
		remote: filepath.Join(root, "origin.git"), lanes: filepath.Join(root, "lanes"),
		log: filepath.Join(root, "treehouse.log"),
	}
	for _, dir := range []string{h.base, h.repo, h.remote, h.lanes, filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.git(h.repo, "init", "-q", "-b", "main")
	h.git(h.repo, "config", "user.email", "test@example.com")
	h.git(h.repo, "config", "user.name", "Test")
	verify := "#!/bin/sh\nset -eu\ntest \"$(cat feature.txt)\" = delivered\n"
	if err := os.WriteFile(filepath.Join(h.repo, "verify-e2e.sh"), []byte(verify), 0o755); err != nil {
		t.Fatal(err)
	}
	h.git(h.repo, "add", "verify-e2e.sh")
	h.git(h.repo, "commit", "-qm", "base")
	h.git(h.remote, "init", "--bare", "-q")
	h.git(h.repo, "remote", "add", "origin", h.remote)
	h.git(h.repo, "push", "-qu", "origin", "main")
	run(t, h.bin, h.repo, "init", "--data", h.base)
	h.writeSyntheticFlow(options)
	h.installTreehouseShim(options)
	h.installCodexShim(options)
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WT_E2E_REPO", h.repo)
	t.Setenv("WT_E2E_LANES", h.lanes)
	t.Setenv("WT_E2E_LOG", h.log)
	t.Setenv("WT_E2E_MODE", options.mode)
	if options.mode == "publish-fail" {
		h.git(h.repo, "remote", "set-url", "origin", filepath.Join(root, "missing-origin.git"))
	}
	t.Cleanup(func() {
		stopDaemons(h.base)
		_ = os.RemoveAll(root)
	})
	return h
}

func (h *flowE2E) git(dir string, args ...string) string {
	h.t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	body, err := command.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git -C %s %s: %v: %s", dir, strings.Join(args, " "), err, body)
	}
	return string(body)
}

func (h *flowE2E) createIssue(title string) string {
	h.t.Helper()
	id := strings.TrimSpace(lastLine(run(h.t, h.bin, h.repo, "new", "--data", h.base,
		"--draft", "--flow", "synthetic", "--title", title)))
	run(h.t, h.bin, h.repo, "launch", "--data", h.base, id)
	return id
}

func (h *flowE2E) socketPath() string {
	return filepath.Join(repocfg.RepoDataDir(h.base, h.repo), "watchtower.sock")
}

func (h *flowE2E) tryIssueDetail(issueID string) (*proto.IssueDetail, error) {
	client, err := proto.Dial(h.socketPath())
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := client.Do(proto.Command{Op: "issue_detail", IssueID: issueID})
	if err != nil {
		return nil, err
	}
	if !response.OK || response.Detail == nil {
		return nil, fmt.Errorf("issue detail: %s", response.Error)
	}
	return response.Detail, nil
}

func (h *flowE2E) issueDetail(issueID string) *proto.IssueDetail {
	h.t.Helper()
	detail, err := h.tryIssueDetail(issueID)
	if err != nil {
		h.t.Fatalf("issue detail %s: %v\n%s", issueID, err, h.diagnostics(issueID))
	}
	return detail
}

func (h *flowE2E) waitForIssueState(issueID, state string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if detail, err := h.tryIssueDetail(issueID); err == nil && detail.Issue.State == state {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("issue %s did not reach %q\n%s", issueID, state, h.diagnostics(issueID))
}

func (h *flowE2E) waitForIntegrationState(issueID, state string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if detail, err := h.tryIssueDetail(issueID); err == nil && detail.IntegrationState == state {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("issue %s integration did not reach %q\n%s", issueID, state, h.diagnostics(issueID))
}

func (h *flowE2E) eventExists(issueID, eventType string) bool {
	client, err := proto.Dial(h.socketPath())
	if err != nil {
		return false
	}
	defer client.Close()
	response, err := client.Do(proto.Command{Op: "tail", SinceSeq: 0})
	if err != nil || !response.OK {
		return false
	}
	for _, event := range response.Events {
		if event.IssueID == issueID && string(event.Type) == eventType {
			return true
		}
	}
	return false
}

func (h *flowE2E) mergeCommitCount() int {
	h.t.Helper()
	count := strings.TrimSpace(h.git(h.repo, "rev-list", "--count", "main"))
	value, err := strconv.Atoi(count)
	if err != nil {
		h.t.Fatalf("merge commit count %q: %v", count, err)
	}
	return value
}

func (h *flowE2E) repairOrigin() {
	h.t.Helper()
	h.git(h.repo, "remote", "set-url", "origin", h.remote)
}

func (h *flowE2E) waitForFile(path string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("file %s did not appear\n%s", path, h.diagnostics(""))
}

func (h *flowE2E) killDaemon() {
	h.t.Helper()
	pidPath := filepath.Join(repocfg.RepoDataDir(h.base, h.repo), "daemon.pid")
	body, err := os.ReadFile(pidPath)
	if err != nil {
		h.t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		h.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("daemon %d did not stop", pid)
}

func (h *flowE2E) diagnostics(issueID string) string {
	var out strings.Builder
	if issueID != "" {
		if detail, err := h.tryIssueDetail(issueID); err != nil {
			fmt.Fprintf(&out, "issue detail (err=%v)\n", err)
		} else {
			fmt.Fprintf(&out, "issue detail: state=%s integration=%s error=%q runs=%+v\n",
				detail.Issue.State, detail.IntegrationState, detail.LastError, detail.Runs)
		}
	}
	for _, args := range [][]string{
		{"issues", "--data", h.base, "--json"},
		{"tail", "--data", h.base},
	} {
		command := exec.Command(h.bin, args...)
		command.Dir = h.repo
		body, err := command.CombinedOutput()
		fmt.Fprintf(&out, "watchtower %s (err=%v):\n%s\n", strings.Join(args, " "), err, body)
	}
	for _, args := range [][]string{
		{"status", "--short", "--branch"},
		{"branch", "-avv"},
		{"worktree", "list", "--porcelain"},
		{"log", "--oneline", "--decorate", "-12"},
	} {
		command := exec.Command("git", append([]string{"-C", h.repo}, args...)...)
		body, err := command.CombinedOutput()
		fmt.Fprintf(&out, "git %s (err=%v):\n%s\n", strings.Join(args, " "), err, body)
	}
	logs, _ := filepath.Glob(filepath.Join(h.base, "repos", "*", "daemon.log"))
	for _, path := range logs {
		body, _ := os.ReadFile(path)
		fmt.Fprintf(&out, "%s:\n%s\n", path, tailBytes(body, 16<<10))
	}
	if body, err := os.ReadFile(h.log); err == nil {
		fmt.Fprintf(&out, "%s:\n%s\n", h.log, body)
	}
	if issueID != "" {
		fmt.Fprintf(&out, "issue: %s\n", issueID)
	}
	return out.String()
}

func tailBytes(body []byte, limit int) string {
	if len(body) > limit {
		body = body[len(body)-limit:]
	}
	return string(body)
}

func (h *flowE2E) writeSyntheticFlow(_ e2eOptions) {
	h.t.Helper()
	flowBody := `name: synthetic
stages:
  - name: prepare-input
    agents: [{package: preparer}]
    workspace: worktree
    gate: auto
    artifacts: [prepared.txt]
  - name: change-repository
    agents: [{package: changer}]
    workspace: worktree
    gate: auto
  - name: integrate-safely
    agents: [{package: integrator}]
    workspace: worktree
    gate: auto
    merge_barrier: true
    artifacts: [merge-report.md, merge-decision.json, verification.json]
`
	watchtower := filepath.Join(h.repo, ".watchtower")
	if err := os.WriteFile(filepath.Join(watchtower, "flows", "default.yaml"), []byte(flowBody), 0o644); err != nil {
		h.t.Fatal(err)
	}
	config := fmt.Sprintf(`runner: codex
codex_bin: %s
codex_model: test-model
codex_effort: low
test_cmd: ./verify-e2e.sh
pull: false
push: true
`, filepath.Join(h.root, "bin", "codex-e2e"))
	if err := os.WriteFile(filepath.Join(watchtower, "config.yaml"), []byte(config), 0o644); err != nil {
		h.t.Fatal(err)
	}
	identities := map[string][3]string{
		"preparer":   {"Input Preparer", "green", "◈"},
		"changer":    {"Repository Changer", "blue", "✚"},
		"integrator": {"Safe Integrator", "orange", "⛨"},
	}
	for _, name := range []string{"preparer", "changer", "integrator"} {
		dir := filepath.Join(watchtower, "packages", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.t.Fatal(err)
		}
		identity := identities[name]
		packageBody := fmt.Sprintf("identity:\n  name: %s\n  color: %s\n  symbol: %s\nallowed_tools: [Bash, Read, Write]\neffort: low\n",
			identity[0], identity[1], identity[2])
		if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(packageBody), 0o644); err != nil {
			h.t.Fatal(err)
		}
		prompt := "Follow STAGE.md and produce only the declared outputs.\n"
		if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *flowE2E) installTreehouseShim(_ e2eOptions) {
	h.t.Helper()
	body := `#!/bin/sh
set -eu

case "$1" in
  get)
    holder=""
    shift
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--lease-holder" ]; then
        holder="$2"
        shift 2
      else
        shift
      fi
    done
    lane="$WT_E2E_LANES/$holder"
    git -C "$WT_E2E_REPO" worktree add -q --detach "$lane" HEAD
    printf 'get %s\n' "$lane" >> "$WT_E2E_LOG"
    printf '%s\n' "$lane"
    ;;
  return)
    shift
    if [ "${1:-}" = "--force" ]; then shift; fi
    lane="$1"
    if [ "${WT_E2E_MODE:-}" = "cleanup-fail-once" ] && [ ! -f "$WT_E2E_LANES/.return-failed" ]; then
      touch "$WT_E2E_LANES/.return-failed"
      exit 1
    fi
    git -C "$WT_E2E_REPO" worktree remove --force "$lane"
    printf 'return %s\n' "$lane" >> "$WT_E2E_LOG"
    ;;
  *)
    exit 2
    ;;
esac
`
	h.writeExecutable(filepath.Join(h.root, "bin", "treehouse"), body)
}

func (h *flowE2E) installCodexShim(_ e2eOptions) {
	h.t.Helper()
	body := `#!/bin/sh
set -eu

stage=$(sed -n 's/^- Stage: //p' STAGE.md | head -1)
base=$(sed -n 's/^- Base commit: //p' STAGE.md | head -1)

case "$stage" in
  prepare-input)
    printf 'prepared\n' > prepared.txt
    ;;
  change-repository)
	printf 'codex change start pid=%s parent=%s\n' "$$" "$PPID" >> "$WT_E2E_LOG"
    if [ "${WT_E2E_MODE:-}" = "pause-change" ] && [ ! -f "$WT_E2E_LANES/change-released" ]; then
      touch "$WT_E2E_LANES/change-started"
      parent=$PPID
      while [ ! -f "$WT_E2E_LANES/change-released" ]; do
		if ! kill -0 "$parent" 2>/dev/null; then
		  printf 'codex change parent-dead pid=%s parent=%s\n' "$$" "$parent" >> "$WT_E2E_LOG"
		  touch "$WT_E2E_LANES/change-aborted"
		  exit 143
		fi
        sleep 0.02
      done
		printf 'codex change released pid=%s parent=%s\n' "$$" "$parent" >> "$WT_E2E_LOG"
    fi
    printf 'delivered\n' > feature.txt
    git add feature.txt
    git commit -qm 'test: deliver synthetic change'
	printf 'codex change committed pid=%s parent=%s\n' "$$" "$PPID" >> "$WT_E2E_LOG"
    ;;
  integrate-safely)
    ./verify-e2e.sh
    if [ "${WT_E2E_MODE:-}" = "advance-base" ] && [ ! -f "$WT_E2E_LANES/.base-advanced" ]; then
      printf 'advanced\n' > "$WT_E2E_REPO/base-advanced.txt"
      git -C "$WT_E2E_REPO" add base-advanced.txt
      git -C "$WT_E2E_REPO" commit -qm 'test: advance base'
      touch "$WT_E2E_LANES/.base-advanced"
    fi
    branch=$(git rev-parse HEAD)
    tree=$(git rev-parse 'HEAD^{tree}')
    printf 'verification passed\n' > merge-report.md
    printf '{"decision":"merge","branch_commit":"%s","base_commit":"%s"}\n' "$branch" "$base" > merge-decision.json
    printf '{"base_sha":"%s","branch_sha":"%s","tree_sha":"%s","passed":true,"commands":[["./verify-e2e.sh"]]}\n' "$base" "$branch" "$tree" > verification.json
    ;;
  *)
    printf 'unexpected stage %s\n' "$stage" >&2
    exit 3
    ;;
esac

safe_stage=$(printf '%s' "$stage" | tr -cd 'A-Za-z0-9_-')
printf '{"type":"thread.started","thread_id":"thr-%s"}\n' "$safe_stage"
printf '{"type":"item.completed","item":{"type":"agent_message","text":"synthetic stage complete"}}\n'
printf '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}\n'
`
	h.writeExecutable(filepath.Join(h.root, "bin", "codex-e2e"), body)
}

func (h *flowE2E) writeExecutable(path, body string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *flowE2E) treehouseReturnCount() int {
	body, _ := os.ReadFile(h.log)
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "return ") {
			count++
		}
	}
	return count
}

func (h *flowE2E) leasedWorktree(issueID string) string {
	return filepath.Join(h.lanes, issueID)
}
