package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/wbushyeager/guildhall/internal/claude"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/engine"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/pkgs"
	"github.com/wbushyeager/guildhall/internal/proto"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/steward"
	"github.com/wbushyeager/guildhall/internal/store"
	"github.com/wbushyeager/guildhall/internal/workspace"
)

func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "guildhall")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: guildhall <daemon|new|decisions|answer|proposals|accept-proposal|reject-proposal|issues|tail> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "daemon":
		runDaemon(args)
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
	claudeBin := fs.String("claude-bin", "claude", "claude binary")
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
	eng := engine.New(engine.Config{
		Store: st, Runner: run, Pool: slots.NewPool(*slotN),
		Flows: flows, DataDir: filepath.Join(*data, "issues"),
		Workspace: ws, TokenBudget: *budget,
		Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe},
	})
	switch r := run.(type) {
	case *claude.CodeRunner:
		r.OnProposal = func(issueID string, p runner.Proposal) {
			eng.FileProposal(issueID, p.Title, p.Body)
		}
	case *runner.FakeRunner:
		r.OnProposal = func(issueID string, p runner.Proposal) {
			eng.FileProposal(issueID, p.Title, p.Body)
		}
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
	fatal(srv.Serve(l))
}

// fakeForFlows builds a FakeRunner that succeeds every stage and writes
// every declared artifact — enough to exercise the pipeline end to end.
func fakeForFlows(flows map[string]flow.Flow) *runner.FakeRunner {
	scripts := map[string]runner.Script{}
	for _, f := range flows {
		for _, st := range f.Stages {
			arts := map[string]string{}
			for _, a := range st.Artifacts {
				arts[a] = ""
			}
			for _, ag := range st.Agents {
				scripts[st.Name+"/"+ag.Package] = runner.Script{Artifacts: arts, Tokens: 10}
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
