package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/weston6142/watchtower/internal/attach"
	"github.com/weston6142/watchtower/internal/claude"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/herdr"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/priority"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/scaffold"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/steward"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/transcript"
	"github.com/weston6142/watchtower/internal/tui"
	"github.com/weston6142/watchtower/internal/workspace"
)

func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "watchtower")
}

func main() {
	if err := migrateStateDir(); err != nil {
		fatal(err)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: watchtower <daemon|init|repos|tower|new|backlog|launch|decisions|answer|proposals|accept-proposal|reject-proposal|issues|status|pause|resume|kill|retry|abandon|lever|transcript|tail> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		created, skipped, err := scaffold.Init(cwd)
		if err != nil {
			fatal(err)
		}
		if err := repocfg.Register(*data, cwd); err != nil {
			fatal(err)
		}
		for _, p := range created {
			fmt.Println("created .watchtower/" + p)
		}
		for _, p := range skipped {
			fmt.Println("exists  .watchtower/" + p)
		}
		fmt.Println("registered", cwd)
		fmt.Println("next: run 'watchtower tower' — the daemon starts automatically")
	case "repos":
		fs := flag.NewFlagSet("repos", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		entries, err := repocfg.ListRegistered(*data)
		if err != nil {
			fatal(err)
		}
		for _, e := range entries {
			status := "stopped"
			sock := filepath.Join(repocfg.RepoDataDir(*data, e.Path), sockFileName)
			if conn, err := net.Dial("unix", sock); err == nil {
				conn.Close()
				status = "running"
			}
			fmt.Printf("%-8s %s\n", status, e.Path)
		}
	case "daemon":
		runDaemon(args)
	case "tower":
		fs := flag.NewFlagSet("tower", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repo := fs.String("repo", "", "repository path for the architecture map")
		stageAliases := fs.String("stage-aliases", "", "display aliases, e.g. spec=AGREE,execute=BUILD")
		reducedMotion := fs.Bool("reduced-motion", false, "disable spinner and failure motion")
		retireAfter := fs.Duration("retire-after", 5*time.Minute, "auto-retire shipped lanes after this duration")
		fs.Parse(args)
		c := mustDial(*data, *repo)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "get_flow", Flow: "default"})
		model := tui.NewModel(c, r.FlowStages)
		reporter := herdr.NewFromEnv()
		model.SetHerdrReporter(reporter)
		model.Repo = *repo
		model.SetStageAliases(tui.ParseStageAliases(*stageAliases))
		model.SetReducedMotion(*reducedMotion)
		model.SetRetireAfter(*retireAfter)
		if cfg, err := repocfg.Load(resolveRepo(*repo)); err == nil {
			tui.SetTheme(cfg.Theme) // empty or unknown falls back to tokyo-night
		}
		_, runErr := tea.NewProgram(model, tea.WithAltScreen()).Run()
		reporter.Idle()
		if runErr != nil {
			fatal(runErr)
		}
	case "snap":
		// Hidden dev command: render every TUI flow from fixtures for
		// visual verification and golden regeneration. Not in usage text.
		fs := flag.NewFlagSet("snap", flag.ExitOnError)
		out := fs.String("out", "tmp-snaps", "output directory")
		width := fs.Int("width", 200, "render width")
		height := fs.Int("height", 50, "render height")
		only := fs.String("flow", "", "render a single flow")
		fs.Parse(args)
		if err := os.MkdirAll(*out, 0o755); err != nil {
			fatal(err)
		}
		for _, f := range tui.FixtureFlows() {
			if *only != "" && f != *only {
				continue
			}
			path := filepath.Join(*out, f+".txt")
			if err := os.WriteFile(path, []byte(tui.SnapshotFlow(f, *width, *height)), 0o644); err != nil {
				fatal(err)
			}
			fmt.Println(path)
		}
	case "new":
		fs := flag.NewFlagSet("new", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		title := fs.String("title", "", "issue title")
		body := fs.String("body", "", "issue body")
		flowName := fs.String("flow", "default", "flow name")
		preset := fs.String("preset", "regular", "yolo|regular|strict")
		prio := fs.Int("priority", 0, "priority")
		draft := fs.Bool("draft", false, "save to the backlog instead of starting")
		var attachments attachFlag
		fs.Var(&attachments, "attach", "attach a file to the issue (repeatable)")
		fs.Parse(args)
		resolved := mustResolveAttach(attachments)
		c := mustDial(*data, *repoF)
		defer c.Close()
		if *draft {
			r := mustDo(c, proto.Command{Op: "draft_issue", Title: *title, Body: *body,
				Flow: *flowName, Preset: *preset, Priority: *prio, Attach: resolved})
			fmt.Println(r.IssueID)
			break
		}
		r := mustDo(c, proto.Command{Op: "create_issue", Title: *title, Body: *body,
			Flow: *flowName, Preset: *preset, Priority: *prio, Attach: resolved})
		mustDo(c, proto.Command{Op: "start_issue", IssueID: r.IssueID})
		fmt.Println(r.IssueID)
	case "decisions":
		fs := flag.NewFlagSet("decisions", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_decisions"})
		for _, d := range r.Decisions {
			fmt.Printf("[%d] %s/%s: %s\n", d.ID, d.IssueID, d.Stage, d.D.Question)
			for i, o := range d.D.Options {
				mark := "  "
				if i == d.D.Recommended {
					mark = "* "
				}
				fmt.Printf("    %s%d) %s\n", mark, i, o)
			}
		}
	case "answer":
		fs := flag.NewFlagSet("answer", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "usage: watchtower answer <decision-id> <option>")
			os.Exit(2)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			fatal(fmt.Errorf("bad decision-id %q: %w", rest[0], err))
		}
		opt, err := strconv.Atoi(rest[1])
		if err != nil {
			fatal(fmt.Errorf("bad option %q: %w", rest[1], err))
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		mustDo(c, proto.Command{Op: "answer_decision", DecisionID: id, Option: opt})
		fmt.Println("answered")
	case "proposals":
		fs := flag.NewFlagSet("proposals", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_proposals"})
		for _, p := range r.Proposals {
			fmt.Printf("[%d] (from %s) %s — %s\n", p.ID, p.IssueID, p.Title, p.Body)
		}
	case "accept-proposal", "reject-proposal":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "usage: watchtower %s <proposal-id>\n", cmd)
			os.Exit(2)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			fatal(fmt.Errorf("bad proposal-id %q: %w", rest[0], err))
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		accepted := cmd == "accept-proposal"
		r := mustDo(c, proto.Command{Op: "resolve_proposal", ProposalID: id, Accept: accepted, Flow: "default", Preset: "regular"})
		if accepted {
			fmt.Println(r.IssueID)
		} else {
			fmt.Println("rejected")
		}
	case "issues":
		fs := flag.NewFlagSet("issues", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_issues"})
		for _, issue := range r.Issues {
			fmt.Printf("%s  %s  %s\n", issue.ID, issue.State, issue.Title)
		}
	case "backlog":
		fs := flag.NewFlagSet("backlog", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_issues"})
		for _, issue := range r.Issues {
			if issue.State != "backlog" {
				continue
			}
			fmt.Printf("%s  %-7s  %s  %s\n", issue.ID, priority.Label(issue.Priority), issue.Flow, issue.Title)
		}
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "overview"})
		fmt.Println(statusSentence(r.Overview))
	case "pause", "resume", "kill", "retry", "abandon", "launch":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		if len(fs.Args()) != 1 {
			fmt.Fprintf(os.Stderr, "usage: watchtower %s <issue-id>\n", cmd)
			os.Exit(2)
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		ops := map[string]string{
			"pause": "pause_issue", "resume": "resume_issue",
			"kill": "kill_stage", "retry": "retry_stage",
			"abandon": "abandon_issue", "launch": "launch_issue",
		}
		mustDo(c, proto.Command{Op: ops[cmd], IssueID: fs.Args()[0]})
		fmt.Println(cmd, fs.Args()[0])
	case "lever":
		fs := flag.NewFlagSet("lever", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		if len(fs.Args()) != 3 {
			fmt.Fprintln(os.Stderr, "usage: watchtower lever <issue-id> <stage> <yolo|regular|strict>")
			os.Exit(2)
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		mustDo(c, proto.Command{Op: "set_lever", IssueID: fs.Args()[0], Stage: fs.Args()[1], Lever: fs.Args()[2]})
		fmt.Printf("lever %s %s %s\n", fs.Args()[0], fs.Args()[1], fs.Args()[2])
	case "transcript":
		fs := flag.NewFlagSet("transcript", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		n := fs.Int("n", 50, "number of lines")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 3 && (rest[1] == "-n" || rest[1] == "--n") {
			parsed, err := strconv.Atoi(rest[2])
			if err != nil {
				fatal(fmt.Errorf("bad line count %q: %w", rest[2], err))
			}
			*n = parsed
			rest = rest[:1]
		}
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: watchtower transcript <issue-id> [-n 50]")
			os.Exit(2)
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "transcript_tail", IssueID: rest[0], N: *n})
		for _, line := range r.Lines {
			fmt.Println(line)
		}
	case "tail":
		fs := flag.NewFlagSet("tail", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		since := fs.Int64("since", 0, "since seq")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "tail", SinceSeq: *since})
		for _, ev := range r.Events {
			fmt.Printf("%d %s %s %s\n", ev.Seq, ev.At.Format("15:04:05"), ev.IssueID, ev.Type)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	base := fs.String("data", defaultData(), "base data dir")
	flowsDir := fs.String("flows", "", "flows dir (default from config)")
	slotN := fs.Int("slots", 0, "heavy slots (default from config)")
	runnerKind := fs.String("runner", "", "claude|fake (default from config)")
	repoFlag := fs.String("repo", "", "target repo (default: walk up from CWD)")
	pkgDir := fs.String("packages", "", "agent packages dir (default from config)")
	budget := fs.Int("budget", -1, "per-issue token budget (0=off; default from config)")
	pricePerMTok := fs.Float64("price-per-mtok", -1, "estimated dollars per million tokens (0=hide; default from config)")
	claudeBin := fs.String("claude-bin", "", "claude binary (default from config)")
	testCmd := fs.String("test-cmd", "", "merge-train test command (default from config)")
	fs.Parse(args)

	repo := resolveRepo(*repoFlag)
	cfg, err := repocfg.Load(repo)
	if err != nil {
		fatal(err)
	}
	// Explicit flags override config; unset flags take config values.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["flows"] {
		*flowsDir = cfg.Flows
	}
	if !set["packages"] {
		*pkgDir = cfg.Packages
	}
	if !set["runner"] {
		*runnerKind = cfg.Runner
	}
	if !set["slots"] {
		*slotN = cfg.Slots
	}
	if !set["budget"] {
		*budget = cfg.Budget
	}
	if !set["price-per-mtok"] {
		*pricePerMTok = cfg.PricePerMTok
	}
	if !set["claude-bin"] {
		*claudeBin = cfg.ClaudeBin
	}
	if !set["test-cmd"] {
		*testCmd = cfg.TestCmd
	}

	data := repocfg.RepoDataDir(*base, repo)
	if err := os.MkdirAll(data, 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, pidFileName), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "watchtower.db"))
	if err != nil {
		fatal(err)
	}
	flows := map[string]flow.Flow{}
	matches, _ := filepath.Glob(filepath.Join(*flowsDir, "*.yaml"))
	for _, m := range matches {
		f, err := flow.Load(m)
		if err != nil {
			fatal(err)
		}
		flows[f.Name] = f
	}
	if len(flows) == 0 {
		fatal(fmt.Errorf("no flows found in %s (configured in %s)", *flowsDir, repocfg.ConfigPath(repo)))
	}
	if os.Getenv("WATCHTOWER_FAKE") == "1" {
		*runnerKind = "fake"
	}
	packages := map[string]pkgs.Package{}
	var run runner.Runner
	var ws workspace.Provider
	switch *runnerKind {
	case "fake":
		run = fakeForFlows(flows)
	case "claude":
		packages, err = pkgs.LoadDir(*pkgDir)
		if err != nil {
			fatal(err)
		}
		run = &claude.CodeRunner{Bin: *claudeBin, Packages: packages}
		ws = workspace.Detect(repo)
	default:
		fatal(fmt.Errorf("unknown runner %q", *runnerKind))
	}
	seq := marshal.New(func(typ core.EventType, issueID string, payload any) {
		ev, err := core.NewEvent(typ, issueID, payload)
		if err == nil {
			_, _ = st.Append(ev)
		}
	})
	var train *marshal.Train
	var lib *librarian.Librarian
	var reconcile func(context.Context, string) error
	if *runnerKind == "claude" {
		resolve := func(ctx context.Context, issueID, branch string) error {
			if ws == nil {
				return fmt.Errorf("no workspace provider for conflict repair")
			}
			wt, release, err := ws.Acquire(issueID + "-repair")
			if err != nil {
				return err
			}
			defer release()
			out, err := exec.Command("git", "-C", wt, "checkout", branch).CombinedOutput()
			if err != nil {
				return fmt.Errorf("checkout: %v: %s", err, out)
			}
			res := <-run.Run(ctx, issueID, "conflict-repair", "conflict-resolver", wt, autoAnswerAsks())
			return res.Err
		}
		train = &marshal.Train{Repo: repo, TestCmd: splitTestCmd(*testCmd), Resolve: resolve,
			Pull: cfg.Pull, Push: cfg.Push}
		lib = &librarian.Librarian{MemoryDir: filepath.Join(repo, "docs", "watchtower")}
		reconcile = func(ctx context.Context, issueID string) error {
			res := <-run.Run(ctx, issueID, "librarian", "librarian", repo, autoAnswerAsks())
			return res.Err
		}
	}
	transcriptBuffer := transcript.NewBuffer(500)
	eng := engine.New(engine.Config{
		Store: st, Runner: run, Pool: slots.NewPool(*slotN),
		Flows: flows, DataDir: filepath.Join(data, "issues"),
		Workspace: ws, TokenBudget: *budget,
		Marshal: seq, Train: train,
		Librarian: lib, Reconcile: reconcile,
		OnLine:    transcriptBuffer.Add,
		Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe},
	})
	// A restart loses all in-memory engine state; rebuild it from the store
	// so pre-restart issues stay controllable. Non-fatal: a failed rehydrate
	// must not stop the daemon from serving new issues.
	if err := eng.Rehydrate(); err != nil {
		fmt.Fprintln(os.Stderr, "rehydrate:", err)
	}
	fileProposal := func(issueID string, p runner.Proposal) {
		eng.FileProposal(issueID, p.Title, p.Body)
	}
	switch r := run.(type) {
	case *claude.CodeRunner:
		r.OnProposal = fileProposal
	case *runner.FakeRunner:
		r.OnProposal = fileProposal
	}
	sock := filepath.Join(data, sockFileName)
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		fatal(err)
	}
	fmt.Println("watchtower daemon listening on", sock)
	srv := proto.NewServer(eng, st)
	srv.SetFlows(flows)
	srv.SetPackages(packages)
	srv.SetTranscript(transcriptBuffer)
	srv.SetPricePerMTok(*pricePerMTok)
	srv.SetBudget(*budget)
	fatal(srv.Serve(l))
}

// autoAnswerAsks returns an Ask channel whose decisions are answered with the
// agent's own recommendation — for maintenance runs (conflict repair, doc
// reconcile) that never escalate to a human.
func autoAnswerAsks() chan runner.Ask {
	asks := make(chan runner.Ask)
	go func() {
		for a := range asks {
			a.Reply <- a.Decision.Recommended
		}
	}()
	return asks
}

func splitTestCmd(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Fields(s)
}

func statusSentence(o *proto.Overview) string {
	if o == nil {
		return ""
	}
	color := "\033[32m"
	if o.Failing > 0 {
		color = "\033[31m"
	} else if o.NeedYou > 0 {
		color = "\033[33m"
	}
	reset := "\033[0m"
	var attention []string
	if o.Failing > 0 {
		attention = append(attention, fmt.Sprintf("%d failing", o.Failing))
	}
	if o.NeedYou > 0 {
		word := "question"
		if o.NeedYou != 1 {
			word = "questions"
		}
		attention = append(attention, fmt.Sprintf("%d %s for you", o.NeedYou, word))
	}
	if len(attention) == 0 {
		attention = append(attention, "all clear")
	}
	cost := ""
	if o.DollarsTotal > 0 {
		cost = fmt.Sprintf(" (~$%.2f)", o.DollarsTotal)
	}
	return fmt.Sprintf("%s●%s %s — %d building, %d shipped today · %s tokens%s",
		color, reset, strings.Join(attention, ", "), o.Building, o.ShippedToday,
		formatTokens(o.TokensTotal), cost)
}

func formatTokens(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fk", float64(tokens)/1000)
	}
	return strconv.Itoa(tokens)
}

// fakeForFlows builds a FakeRunner that succeeds every stage and writes
// every declared artifact — enough to exercise the pipeline end to end.
func fakeForFlows(flows map[string]flow.Flow) *runner.FakeRunner {
	scripts := map[string]runner.Script{}
	for _, f := range flows {
		for _, st := range f.Stages {
			arts := map[string]string{}
			for _, a := range st.Artifacts {
				content := ""
				if a == "touchset.json" {
					content = `{"globs":["src/**"]}`
				}
				arts[a] = content
			}
			for _, ag := range st.Agents {
				scripts[st.Name+"/"+ag.Package] = runner.Script{
					Artifacts: arts, Tokens: 10,
					Lines: []string{fmt.Sprintf("fake %s/%s complete", st.Name, ag.Package)},
				}
			}
		}
	}
	return &runner.FakeRunner{Scripts: scripts}
}

// attachFlag collects a repeatable --attach.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

func (a *attachFlag) Set(value string) error {
	*a = append(*a, value)
	return nil
}

// mustResolveAttach resolves each --attach against the user's cwd before the
// path goes on the wire: the daemon does not run here and must not guess. A
// new issue has no existing attachments, so every entry is a path.
func mustResolveAttach(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		resolved, err := attach.ResolveEntry(entry, nil, cwd, home)
		if err != nil {
			fatal(err)
		}
		if resolved != "" {
			out = append(out, resolved)
		}
	}
	return out
}

func mustDo(c *proto.Client, cmd proto.Command) proto.Response {
	r, err := c.Do(cmd)
	if err != nil {
		fatal(err)
	}
	if !r.OK {
		fatal(fmt.Errorf("%s: %s", cmd.Op, r.Error))
	}
	return r
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "watchtower:", err)
		os.Exit(1)
	}
}
