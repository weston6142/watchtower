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

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wbushyeager/guildhall/internal/claude"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/engine"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/librarian"
	"github.com/wbushyeager/guildhall/internal/marshal"
	"github.com/wbushyeager/guildhall/internal/pkgs"
	"github.com/wbushyeager/guildhall/internal/proto"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/steward"
	"github.com/wbushyeager/guildhall/internal/store"
	"github.com/wbushyeager/guildhall/internal/transcript"
	"github.com/wbushyeager/guildhall/internal/tui"
	"github.com/wbushyeager/guildhall/internal/workspace"
)

func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "guildhall")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: guildhall <daemon|tower|new|decisions|answer|proposals|accept-proposal|reject-proposal|issues|status|pause|resume|kill|retry|lever|transcript|tail> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "daemon":
		runDaemon(args)
	case "tower":
		fs := flag.NewFlagSet("tower", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repo := fs.String("repo", "", "repository path for the architecture map")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "get_flow", Flow: "default"})
		model := tui.NewModel(c, r.FlowStages)
		model.Repo = *repo
		if _, err := tea.NewProgram(model, tea.WithAltScreen()).Run(); err != nil {
			fatal(err)
		}
	case "new":
		fs := flag.NewFlagSet("new", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		title := fs.String("title", "", "issue title")
		flowName := fs.String("flow", "default", "flow name")
		preset := fs.String("preset", "regular", "yolo|regular|strict")
		prio := fs.Int("priority", 0, "priority")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "create_issue", Title: *title, Flow: *flowName, Preset: *preset, Priority: *prio})
		mustDo(c, proto.Command{Op: "start_issue", IssueID: r.IssueID})
		fmt.Println(r.IssueID)
	case "decisions":
		fs := flag.NewFlagSet("decisions", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		c := mustDial(*data)
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
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "usage: guildhall answer <decision-id> <option>")
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
		c := mustDial(*data)
		defer c.Close()
		mustDo(c, proto.Command{Op: "answer_decision", DecisionID: id, Option: opt})
		fmt.Println("answered")
	case "proposals":
		fs := flag.NewFlagSet("proposals", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_proposals"})
		for _, p := range r.Proposals {
			fmt.Printf("[%d] (from %s) %s — %s\n", p.ID, p.IssueID, p.Title, p.Body)
		}
	case "accept-proposal", "reject-proposal":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "usage: guildhall %s <proposal-id>\n", cmd)
			os.Exit(2)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			fatal(fmt.Errorf("bad proposal-id %q: %w", rest[0], err))
		}
		c := mustDial(*data)
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
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_issues"})
		for _, issue := range r.Issues {
			fmt.Printf("%s  %s  %s\n", issue.ID, issue.State, issue.Title)
		}
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "overview"})
		fmt.Println(statusSentence(r.Overview))
	case "pause", "resume", "kill", "retry":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		if len(fs.Args()) != 1 {
			fmt.Fprintf(os.Stderr, "usage: guildhall %s <issue-id>\n", cmd)
			os.Exit(2)
		}
		c := mustDial(*data)
		defer c.Close()
		ops := map[string]string{
			"pause": "pause_issue", "resume": "resume_issue",
			"kill": "kill_stage", "retry": "retry_stage",
		}
		mustDo(c, proto.Command{Op: ops[cmd], IssueID: fs.Args()[0]})
		fmt.Println(cmd, fs.Args()[0])
	case "lever":
		fs := flag.NewFlagSet("lever", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		if len(fs.Args()) != 3 {
			fmt.Fprintln(os.Stderr, "usage: guildhall lever <issue-id> <stage> <yolo|regular|strict>")
			os.Exit(2)
		}
		c := mustDial(*data)
		defer c.Close()
		mustDo(c, proto.Command{Op: "set_lever", IssueID: fs.Args()[0], Stage: fs.Args()[1], Lever: fs.Args()[2]})
		fmt.Printf("lever %s %s %s\n", fs.Args()[0], fs.Args()[1], fs.Args()[2])
	case "transcript":
		fs := flag.NewFlagSet("transcript", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
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
			fmt.Fprintln(os.Stderr, "usage: guildhall transcript <issue-id> [-n 50]")
			os.Exit(2)
		}
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "transcript_tail", IssueID: rest[0], N: *n})
		for _, line := range r.Lines {
			fmt.Println(line)
		}
	case "tail":
		fs := flag.NewFlagSet("tail", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		since := fs.Int64("since", 0, "since seq")
		fs.Parse(args)
		c := mustDial(*data)
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
	data := fs.String("data", defaultData(), "data dir")
	flowsDir := fs.String("flows", "", "flows dir (required)")
	slotN := fs.Int("slots", 4, "heavy slots")
	runnerKind := fs.String("runner", "claude", "claude|fake")
	repo := fs.String("repo", "", "target repo (required for --runner claude)")
	pkgDir := fs.String("packages", "dist/packages", "agent packages dir")
	budget := fs.Int("budget", 0, "per-issue token budget (0=off)")
	pricePerMTok := fs.Float64("price-per-mtok", 0, "estimated dollars per million tokens (0=hide)")
	claudeBin := fs.String("claude-bin", "claude", "claude binary")
	testCmd := fs.String("test-cmd", "", "merge-train test command")
	fs.Parse(args)
	if *flowsDir == "" {
		fmt.Fprintln(os.Stderr, "daemon: --flows is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		fatal(err)
	}
	st, err := store.Open(filepath.Join(*data, "guildhall.db"))
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
		fatal(fmt.Errorf("no flows found in %s", *flowsDir))
	}
	if os.Getenv("GUILDHALL_FAKE") == "1" {
		*runnerKind = "fake"
	}
	var run runner.Runner
	var ws workspace.Provider
	switch *runnerKind {
	case "fake":
		run = fakeForFlows(flows)
	case "claude":
		if *repo == "" {
			fatal(fmt.Errorf("--repo is required with --runner claude"))
		}
		packages, err := pkgs.LoadDir(*pkgDir)
		if err != nil {
			fatal(err)
		}
		run = &claude.CodeRunner{Bin: *claudeBin, Packages: packages}
		ws = workspace.Detect(*repo)
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
		train = &marshal.Train{Repo: *repo, TestCmd: splitTestCmd(*testCmd), Resolve: resolve}
		lib = &librarian.Librarian{MemoryDir: filepath.Join(*repo, "docs", "guildhall")}
		reconcile = func(ctx context.Context, issueID string) error {
			res := <-run.Run(ctx, issueID, "librarian", "librarian", *repo, autoAnswerAsks())
			return res.Err
		}
	}
	transcriptBuffer := transcript.NewBuffer(500)
	eng := engine.New(engine.Config{
		Store: st, Runner: run, Pool: slots.NewPool(*slotN),
		Flows: flows, DataDir: filepath.Join(*data, "issues"),
		Workspace: ws, TokenBudget: *budget,
		Marshal: seq, Train: train,
		Librarian: lib, Reconcile: reconcile,
		OnLine:    transcriptBuffer.Add,
		Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe},
	})
	fileProposal := func(issueID string, p runner.Proposal) {
		eng.FileProposal(issueID, p.Title, p.Body)
	}
	switch r := run.(type) {
	case *claude.CodeRunner:
		r.OnProposal = fileProposal
	case *runner.FakeRunner:
		r.OnProposal = fileProposal
	}
	sock := filepath.Join(*data, "guildhall.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		fatal(err)
	}
	fmt.Println("guildhall daemon listening on", sock)
	srv := proto.NewServer(eng, st)
	srv.SetFlows(flows)
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

func mustDial(data string) *proto.Client {
	c, err := proto.Dial(filepath.Join(data, "guildhall.sock"))
	if err != nil {
		fatal(err)
	}
	return c
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
		fmt.Fprintln(os.Stderr, "guildhall:", err)
		os.Exit(1)
	}
}
