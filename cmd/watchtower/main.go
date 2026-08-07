package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/weston6142/watchtower/internal/attach"
	"github.com/weston6142/watchtower/internal/claude"
	"github.com/weston6142/watchtower/internal/codex"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/herdr"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/priority"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/review"
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

type plannerBudgetFlags struct {
	callsWarn, callsHard     int64
	tokensWarn, tokensHard   int64
	elapsedWarn, elapsedHard time.Duration
}

func bindPlannerBudgetFlags(fs *flag.FlagSet) *plannerBudgetFlags {
	values := &plannerBudgetFlags{}
	fs.Int64Var(&values.callsWarn, "planner-calls-warn", -1, "planner calls warning threshold")
	fs.Int64Var(&values.callsHard, "planner-calls-hard", -1, "planner calls hard threshold")
	fs.Int64Var(&values.tokensWarn, "planner-tokens-warn", -1, "planner tokens warning threshold")
	fs.Int64Var(&values.tokensHard, "planner-tokens-hard", -1, "planner tokens hard threshold")
	fs.DurationVar(&values.elapsedWarn, "planner-elapsed-warn", 0, "planner elapsed warning threshold")
	fs.DurationVar(&values.elapsedHard, "planner-elapsed-hard", 0, "planner elapsed hard threshold")
	return values
}

func plannerIntegerOverride(set map[string]bool, warnName, hardName string, warning, hard int64) *plannerbudget.DimensionOverride {
	if !set[warnName] && !set[hardName] {
		return nil
	}
	override := &plannerbudget.DimensionOverride{}
	if set[warnName] {
		override.Warning = &warning
	}
	if set[hardName] {
		override.Hard = &hard
	}
	return override
}

func plannerElapsedOverride(set map[string]bool, warnName, hardName string, warning, hard time.Duration) *plannerbudget.ElapsedOverride {
	if !set[warnName] && !set[hardName] {
		return nil
	}
	override := &plannerbudget.ElapsedOverride{}
	if set[warnName] {
		override.Warning = &warning
	}
	if set[hardName] {
		override.Hard = &hard
	}
	return override
}

func plannerBudgetOverride(fs *flag.FlagSet, values *plannerBudgetFlags) *plannerbudget.Override {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	override := &plannerbudget.Override{
		Calls:   plannerIntegerOverride(set, "planner-calls-warn", "planner-calls-hard", values.callsWarn, values.callsHard),
		Tokens:  plannerIntegerOverride(set, "planner-tokens-warn", "planner-tokens-hard", values.tokensWarn, values.tokensHard),
		Elapsed: plannerElapsedOverride(set, "planner-elapsed-warn", "planner-elapsed-hard", values.elapsedWarn, values.elapsedHard),
	}
	if override.Calls == nil && override.Tokens == nil && override.Elapsed == nil {
		return nil
	}
	return override
}

func main() {
	if err := migrateStateDir(); err != nil {
		fatal(err)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: watchtower <daemon|stop|init|reset|repos|tower|new|backlog|claim|release|finish|launch|decisions|answer|proposals|accept-proposal|reject-proposal|issues|status|pause|resume|kill|retry|abandon|lever|transcript|tail|planner-artifact> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "planner-artifact":
		if err := runPlannerArtifact(append([]string{cmd}, args...), os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}
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
	case "reset":
		if err := runReset(args); err != nil {
			fatal(err)
		}
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
	case "stop":
		if err := runStop(args); err != nil {
			fatal(err)
		}
	case "tower":
		fs := flag.NewFlagSet("tower", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repo := fs.String("repo", "", "repository path for the architecture map")
		stageAliases := fs.String("stage-aliases", "", "display aliases, e.g. spec=AGREE,execute=BUILD")
		reducedMotion := fs.Bool("reduced-motion", false, "disable spinner and failure motion")
		retireAfter := fs.Duration("retire-after", 5*time.Minute, "auto-retire shipped lanes after this duration")
		fs.Parse(args)
		repoRoot := resolveRepo(*repo)
		c := mustDial(*data, *repo)
		r := mustDo(c, proto.Command{Op: "get_flow", Flow: "default"})
		model := tui.NewModel(c, r.FlowStages)
		model.SetReconnectDialer(func() (tui.Session, error) {
			return dialExistingDaemon(*data, repoRoot)
		})
		reporter := herdr.NewFromEnv()
		model.SetHerdrReporter(reporter)
		model.SetPaneSpawner(reporter)
		model.Repo = repoRoot
		model.SetStageAliases(tui.ParseStageAliases(*stageAliases))
		model.SetReducedMotion(*reducedMotion)
		model.SetRetireAfter(*retireAfter)
		if cfg, err := repocfg.Load(repoRoot); err == nil {
			tui.SetTheme(cfg.Theme) // empty or unknown falls back to tokyo-night
		}
		finalModel, runErr := tea.NewProgram(model, tea.WithAltScreen()).Run()
		if final, ok := finalModel.(tui.Model); ok {
			_ = final.Close()
		} else {
			_ = model.Close()
		}
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
		plannerFlags := bindPlannerBudgetFlags(fs)
		var attachments attachFlag
		var dependsOn stringListFlag
		fs.Var(&attachments, "attach", "attach a file to the issue (repeatable)")
		fs.Var(&dependsOn, "depends-on", "issue that must merge first (repeatable)")
		fs.Parse(args)
		resolved := mustResolveAttach(attachments)
		c := mustDial(*data, *repoF)
		defer c.Close()
		if *draft {
			r := mustDo(c, proto.Command{Op: "draft_issue", Title: *title, Body: *body,
				Flow: *flowName, Preset: *preset, Priority: *prio, Attach: resolved,
				DependsOn: dependsOn})
			fmt.Println(r.IssueID)
			break
		}
		r := mustDo(c, proto.Command{Op: "create_issue", Title: *title, Body: *body,
			Flow: *flowName, Preset: *preset, Priority: *prio, Attach: resolved,
			DependsOn: dependsOn})
		mustDo(c, proto.Command{Op: "start_issue", IssueID: r.IssueID,
			PlannerBudget: plannerBudgetOverride(fs, plannerFlags)})
		fmt.Println(r.IssueID)
	case "decisions":
		fs := flag.NewFlagSet("decisions", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		jsonOut := fs.Bool("json", false, "emit JSON")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_decisions"})
		if *jsonOut {
			printJSON(r.Decisions)
			break
		}
		for _, d := range r.Decisions {
			fmt.Print(formatDecision(d))
		}
	case "answer":
		fs := flag.NewFlagSet("answer", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		textAnswer := fs.String("text", "", "freeform decision response")
		actor := fs.String("actor", review.DefaultActorID, "human actor identity")
		fs.Parse(args)
		rest := fs.Args()
		// The documented form puts --text after the id. The standard flag
		// package stops at the first positional, so recognize that form
		// explicitly while retaining normal flags-before-positionals parsing.
		if len(rest) >= 3 && rest[1] == "--text" {
			*textAnswer = strings.Join(rest[2:], " ")
			rest = rest[:1]
		}
		if len(rest) < 1 || len(rest) > 2 || (len(rest) == 2 && *textAnswer != "") {
			fmt.Fprintln(os.Stderr, "usage: watchtower answer <decision-id> <option> | watchtower answer <decision-id> --text <response> [--actor actor]")
			os.Exit(2)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			fatal(fmt.Errorf("bad decision-id %q: %w", rest[0], err))
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		answer := proto.Command{Op: "answer_decision", DecisionID: id, Text: *textAnswer, Actor: *actor}
		if len(rest) == 2 {
			opt, err := strconv.Atoi(rest[1])
			if err != nil {
				fatal(fmt.Errorf("bad option %q: %w", rest[1], err))
			}
			answer.Option = &opt
		}
		mustDo(c, answer)
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
			dependencySuffix := ""
			if len(p.DependsOn) > 0 {
				dependencySuffix = " [depends on " + strings.Join(p.DependsOn, ", ") + "]"
			}
			fmt.Printf("[%d] (from %s) %s%s — %s\n",
				p.ID, p.IssueID, p.Title, dependencySuffix, p.Body)
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
		jsonOut := fs.Bool("json", false, "emit JSON")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_issues"})
		if *jsonOut {
			printJSON(r.Issues)
			break
		}
		for _, issue := range r.Issues {
			fmt.Printf("%s  %s  %s\n", issue.ID, issue.State, issue.Title)
		}
	case "backlog":
		fs := flag.NewFlagSet("backlog", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		jsonOut := fs.Bool("json", false, "emit JSON")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_backlog"})
		if *jsonOut {
			printJSON(struct {
				Backlog []proto.BacklogItem `json:"backlog"`
				Claims  []engine.Claim      `json:"claims"`
			}{Backlog: r.Backlog, Claims: r.Claims})
			break
		}
		for _, item := range r.Backlog {
			dependencySuffix := ""
			if len(item.BlockedBy) > 0 {
				dependencySuffix = "  depends on " + strings.Join(item.BlockedBy, ", ")
			}
			fmt.Printf("%s  %-7s  %s  %s%s\n",
				item.Issue.ID, priority.Label(item.Issue.Priority), item.Issue.Flow,
				item.Issue.Title, dependencySuffix)
		}
	case "claim", "release":
		filtered, jsonOut := removeFlag(args, "--json")
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(filtered)
		if len(fs.Args()) != 1 {
			fmt.Fprintf(os.Stderr, "usage: watchtower %s <issue-id> [--json]\n", cmd)
			os.Exit(2)
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		issueID := fs.Args()[0]
		op := "claim_issue"
		if cmd == "release" {
			op = "release_claim"
		}
		r := mustDo(c, proto.Command{Op: op, IssueID: issueID})
		if jsonOut {
			if cmd == "claim" {
				printJSON(r.Claim)
			} else {
				printJSON(struct {
					OK      bool   `json:"ok"`
					IssueID string `json:"issue_id"`
				}{OK: true, IssueID: issueID})
			}
			break
		}
		if cmd == "claim" {
			fmt.Printf("claimed %s  %s  %s\n", issueID, r.Claim.Branch, r.Claim.Worktree)
		} else {
			fmt.Println("released", issueID)
		}
	case "finish":
		filtered, jsonOut := removeFlag(args, "--json")
		filtered, allowNoChange := removeFlag(filtered, "--allow-no-change")
		fs := flag.NewFlagSet("finish", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(filtered)
		if len(fs.Args()) > 1 {
			fmt.Fprintln(os.Stderr, "usage: watchtower finish [issue-id] [--allow-no-change] [--json]")
			os.Exit(2)
		}
		worktree, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		worktree, err = filepath.Abs(worktree)
		if err != nil {
			fatal(err)
		}
		c := mustDial(*data, *repoF)
		defer c.Close()
		issueID := ""
		if len(fs.Args()) == 1 {
			issueID = fs.Args()[0]
		} else {
			claim := mustDo(c, proto.Command{Op: "claim_for_worktree", Worktree: worktree})
			issueID = claim.IssueID
		}
		mustDo(c, proto.Command{
			Op: "finish_claim", IssueID: issueID, Worktree: worktree,
			AllowNoChange: allowNoChange,
		})
		if jsonOut {
			printJSON(struct {
				OK      bool   `json:"ok"`
				IssueID string `json:"issue_id"`
			}{OK: true, IssueID: issueID})
			break
		}
		fmt.Println("finalization started", issueID)
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		jsonOut := fs.Bool("json", false, "emit JSON")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "overview"})
		if *jsonOut {
			printJSON(r.Overview)
			break
		}
		fmt.Println(statusSentence(r.Overview))
	case "pause", "resume", "kill", "retry", "abandon", "launch":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		var plannerFlags *plannerBudgetFlags
		if cmd == "retry" || cmd == "launch" {
			plannerFlags = bindPlannerBudgetFlags(fs)
		}
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
		var plannerOverride *plannerbudget.Override
		if plannerFlags != nil {
			plannerOverride = plannerBudgetOverride(fs, plannerFlags)
		}
		mustDo(c, proto.Command{Op: ops[cmd], IssueID: fs.Args()[0], PlannerBudget: plannerOverride})
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

func runReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	data := fs.String("data", defaultData(), "data dir")
	repoFlag := fs.String("repo", "", "target repo (default: walk up from CWD)")
	yes := fs.Bool("yes", false, "replace defaults without confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repo := *repoFlag
	if repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		repo, err = repocfg.FindRepo(cwd)
		if err != nil {
			return err
		}
	}
	currentConfig, err := repocfg.Load(repo)
	if err != nil {
		return err
	}
	prepared, err := scaffold.PrepareReset(repo)
	if err != nil {
		return err
	}
	defer prepared.Cancel()
	if currentConfig.TestCmd != "" {
		if err := prepared.SetTestCommand(currentConfig.TestCmd); err != nil {
			return err
		}
	}

	if !*yes {
		fmt.Fprint(os.Stdout, "Replace .watchtower with the current defaults? [y/N] ")
		answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(os.Stdout, "reset canceled")
			return nil
		}
	}

	client, _, err := connectOrStartDaemon(*data, repo)
	if err != nil {
		return err
	}
	response, err := client.Do(proto.Command{Op: "can_reset"})
	if err != nil {
		client.Close()
		return err
	}
	if !response.OK {
		client.Close()
		return fmt.Errorf("can_reset: %s", response.Error)
	}
	response, err = client.Do(proto.Command{Op: "shutdown"})
	client.Close()
	if err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("shutdown: %s", response.Error)
	}
	if err := waitForDaemonStop(*data, repo); err != nil {
		return err
	}

	if err := prepared.Apply(); err != nil {
		originalClient, _, restartErr := connectOrStartDaemon(*data, repo)
		if originalClient != nil {
			originalClient.Close()
		}
		if restartErr != nil {
			return fmt.Errorf("apply reset: %v; restart original daemon: %w", err, restartErr)
		}
		return err
	}
	newClient, _, startErr := connectOrStartDaemon(*data, repo)
	if startErr == nil {
		response, startErr = newClient.Do(proto.Command{Op: "setup_outline"})
		if startErr == nil && (!response.OK || response.Setup == nil) {
			startErr = fmt.Errorf("setup validation: %s", response.Error)
		}
	}
	if startErr != nil {
		if newClient != nil {
			_, _ = newClient.Do(proto.Command{Op: "shutdown"})
			_ = newClient.Close()
			_ = waitForDaemonStop(*data, repo)
		}
		if rollbackErr := prepared.Rollback(); rollbackErr != nil {
			return fmt.Errorf("new defaults failed: %v; rollback failed: %w", startErr, rollbackErr)
		}
		oldClient, _, restartErr := connectOrStartDaemon(*data, repo)
		if oldClient != nil {
			oldClient.Close()
		}
		if restartErr != nil {
			return fmt.Errorf("new defaults failed: %v; restart original daemon: %w", startErr, restartErr)
		}
		return fmt.Errorf("new defaults failed validation: %w", startErr)
	}
	newClient.Close()
	if err := prepared.Commit(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "replaced .watchtower defaults")
	return nil
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	base := fs.String("data", defaultData(), "base data dir")
	flowsDir := fs.String("flows", "", "flows dir (default from config)")
	slotN := fs.Int("slots", 0, "heavy slots (default from config)")
	runnerKind := fs.String("runner", "", "codex|claude|fake (default from config)")
	repoFlag := fs.String("repo", "", "target repo (default: walk up from CWD)")
	pkgDir := fs.String("packages", "", "agent packages dir (default from config)")
	budget := fs.Int("budget", -1, "per-issue token budget (0=off; default from config)")
	pricePerMTok := fs.Float64("price-per-mtok", -1, "estimated dollars per million tokens (0=hide; default from config)")
	claudeBin := fs.String("claude-bin", "", "claude binary (default from config)")
	codexBin := fs.String("codex-bin", "", "codex binary (default from config)")
	codexModel := fs.String("codex-model", "", "codex model (default from config)")
	codexEffort := fs.String("codex-effort", "", "codex reasoning effort (default from config)")
	testCmd := fs.String("test-cmd", "", "merge-train test command (default from config)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fatal(fmt.Errorf("daemon: unexpected argument %q", fs.Arg(0)))
	}

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
	if !set["codex-bin"] {
		*codexBin = cfg.CodexBin
	}
	if !set["codex-model"] {
		*codexModel = cfg.CodexModel
	}
	if !set["codex-effort"] {
		*codexEffort = cfg.CodexEffort
	}
	if !set["test-cmd"] {
		*testCmd = cfg.TestCmd
	}
	testArgv := append([]string(nil), cfg.TestArgv...)
	if set["test-cmd"] {
		testArgv, err = repocfg.ParseCommand(*testCmd)
		if err != nil {
			fatal(fmt.Errorf("test-cmd: %w", err))
		}
	}

	data := repocfg.RepoDataDir(*base, repo)
	if err := os.MkdirAll(data, 0o755); err != nil {
		fatal(err)
	}
	sock := filepath.Join(data, sockFileName)
	if client, dialErr := proto.Dial(sock); dialErr == nil {
		client.Close()
		fatal(fmt.Errorf("daemon already running for %s", repo))
	}
	clearStaleSocket(data, sock)
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
	for _, configuredFlow := range flows {
		if err := configuredFlow.ValidateIntegration(testArgv); err != nil {
			fatal(err)
		}
	}
	if os.Getenv("WATCHTOWER_FAKE") == "1" {
		*runnerKind = "fake"
	}
	packages, err := pkgs.LoadDir(*pkgDir)
	if err != nil {
		fatal(err)
	}
	decisionIdentities := make(map[string]decision.AgentIdentity, len(packages))
	for name, pkg := range packages {
		decisionIdentities[name] = pkg.Identity
	}
	var run runner.Runner
	var ws workspace.Provider
	primaryCodex := cfg.Codex.Primary
	primaryCodex.Bin, primaryCodex.Model, primaryCodex.Effort = *codexBin, *codexModel, *codexEffort
	var fallbackCodex *repocfg.CodexProfile
	if cfg.Codex.Fallback != nil {
		fallback := *cfg.Codex.Fallback
		fallback.Bin, fallback.Model, fallback.Effort = primaryCodex.Bin, primaryCodex.Model, primaryCodex.Effort
		fallbackCodex = &fallback
	}
	switch *runnerKind {
	case "fake":
		fake := fakeForFlows(flows)
		if _, gitErr := exec.Command("git", "-C", repo, "rev-parse", "--git-dir").Output(); gitErr == nil {
			ws = workspace.GitWorktree{Repo: repo}
			stagesByName := map[string]flow.Stage{}
			for _, configuredFlow := range flows {
				for _, stage := range configuredFlow.Stages {
					stagesByName[stage.Name] = stage
				}
			}
			changed := sync.Map{}
			fake.OnStart = func(issueID, stage, _, workdir string) error {
				stageConfig, ok := stagesByName[stage]
				if !ok || stageConfig.MergeBarrier || stageConfig.Workspace == "none" {
					return nil
				}
				if _, loaded := changed.LoadOrStore(issueID, true); loaded {
					return nil
				}
				return commitFakeChange(issueID, workdir)
			}
		}
		run = fake
	case "claude":
		run = &claude.CodeRunner{Bin: *claudeBin, Packages: packages}
		ws = workspace.Detect(repo)
	case "codex":
		run = &codex.CodeRunner{
			Bin: *codexBin, Packages: packages,
			DefaultModel: *codexModel, DefaultEffort: *codexEffort,
			PrimaryProfile: primaryCodex, FallbackProfile: fallbackCodex,
		}
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
	if ws != nil {
		train = &marshal.Train{Repo: repo, TestCmd: testArgv, CacheRoot: data,
			Pull: cfg.Pull, Push: cfg.Push}
	}
	if *runnerKind != "fake" {
		lib = &librarian.Librarian{MemoryDir: filepath.Join(repo, "docs", "watchtower")}
	}
	transcriptBuffer := transcript.NewBuffer(500)
	eng := engine.New(engine.Config{
		Store: st, Runner: run, Pool: slots.NewPool(*slotN),
		Flows: flows, DataDir: filepath.Join(data, "issues"), CacheRoot: data,
		Workspace: ws, TokenBudget: *budget,
		PlannerBudget:      cfg.PlannerBudget,
		PlanReview:         cfg.PlanReviewSettings(),
		DecisionIdentities: decisionIdentities,
		Marshal:            seq, Train: train,
		Librarian: lib,
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
		eng.FileProposalWithDependencies(issueID, p.Title, p.Body, p.DependsOn)
	}
	switch r := run.(type) {
	case *claude.CodeRunner:
		r.OnProposal = fileProposal
		r.OnProposalBatch = eng.FileProposalBatch
	case *codex.CodeRunner:
		r.OnProposal = fileProposal
		r.OnProposalBatch = eng.FileProposalBatch
	case *runner.FakeRunner:
		r.OnProposal = fileProposal
		r.OnProposalBatch = eng.FileProposalBatch
	}
	if client, dialErr := proto.Dial(sock); dialErr == nil {
		client.Close()
		fatal(fmt.Errorf("daemon already running for %s", repo))
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, pidFileName), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		l.Close()
		fatal(err)
	}
	fmt.Println("watchtower daemon listening on", sock)
	srv := proto.NewServer(eng, st)
	srv.SetFlows(flows)
	setupPackages := packages
	if *runnerKind == "fake" {
		setupPackages = map[string]pkgs.Package{}
	}
	srv.SetPackages(setupPackages)
	srv.SetTranscript(transcriptBuffer)
	srv.SetPricePerMTok(*pricePerMTok)
	srv.SetBudget(*budget)
	srv.SetPlannerBudget(cfg.PlannerBudget)
	// The setup inspector reports what the daemon is running, so these are the
	// post-override values, and the workspace is the provider actually held —
	// workspace.Detect picks treehouse purely on PATH and nothing else can see
	// which one won. LoadedAt is formatted here, once, so no render path calls
	// time.Now() and the golden snapshots stay deterministic.
	wsName := ""
	if ws != nil {
		wsName = ws.Name()
	}
	setup := proto.RepoSetup{
		Runner: *runnerKind, Slots: *slotN, Budget: *budget,
		PricePerMTok: *pricePerMTok, ClaudeBin: *claudeBin, TestCmd: *testCmd,
		CodexBin: *codexBin, CodexModel: *codexModel, CodexEffort: *codexEffort,
		Pull: cfg.Pull, Push: cfg.Push, Workspace: wsName,
		LoadedAt: time.Now().Format("15:04"),
	}
	if *runnerKind == "codex" {
		setup.CodexPolicy = cfg.CodexPolicy()
		setup.CodexPrimary = codexProfileSetup("primary", codex.DescribeCodexProfile(primaryCodex))
		if fallbackCodex != nil {
			setup.CodexFallback = codexProfileSetup("fallback", codex.DescribeCodexProfile(*fallbackCodex))
		}
	}
	srv.SetRepoSetup(setup)
	fatal(srv.Serve(l))
}

func codexProfileSetup(label string, profile codex.CodexProfileSetup) *proto.CodexProfileSetup {
	return &proto.CodexProfileSetup{
		Label: label, Bin: profile.Bin, Model: profile.Model, Effort: profile.Effort,
		FeatureOverrides: profile.FeatureOverrides,
		InitialArgv:      profile.InitialArgv, ResumedArgv: profile.ResumedArgv,
	}
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	base := fs.String("data", defaultData(), "base data dir")
	repoFlag := fs.String("repo", "", "target repo (default: walk up from CWD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("stop: unexpected argument %q", fs.Arg(0))
	}
	repo := resolveRepo(*repoFlag)
	dataDir := repocfg.RepoDataDir(*base, repo)
	sock := filepath.Join(dataDir, sockFileName)
	client, err := proto.Dial(sock)
	if err != nil {
		clearStaleSocket(dataDir, sock)
		fmt.Println("daemon already stopped for", repo)
		return nil
	}
	response, err := client.Do(proto.Command{Op: "shutdown"})
	client.Close()
	if err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("shutdown: %s", response.Error)
	}
	if err := waitForDaemonStop(*base, repo); err != nil {
		return err
	}
	fmt.Println("stopped daemon for", repo)
	return nil
}

func commitFakeChange(issueID, workdir string) error {
	dir := filepath.Join(workdir, "watchtower-fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, issueID+".txt")
	if err := os.WriteFile(path, []byte(issueID+"\n"), 0o644); err != nil {
		return err
	}
	if output, err := exec.Command("git", "-C", workdir, "add", path).CombinedOutput(); err != nil {
		return fmt.Errorf("fake stage add: %v: %s", err, output)
	}
	if output, err := exec.Command(
		"git", "-C", workdir, "commit", "-qm", "fake change "+issueID,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("fake stage commit: %v: %s", err, output)
	}
	return nil
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

func printJSON(value any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fatal(err)
	}
}

func removeFlag(args []string, target string) ([]string, bool) {
	filtered := make([]string, 0, len(args))
	found := false
	for _, arg := range args {
		if arg == target {
			found = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, found
}

func formatTokens(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fk", float64(tokens)/1000)
	}
	return strconv.Itoa(tokens)
}

func formatDecision(d engine.PendingDecision) string {
	var out strings.Builder
	if d.Context != nil {
		fmt.Fprintf(&out, "task: %s\n", d.Context.TaskSummary)
		fmt.Fprintf(&out, "agent: %s\n", d.Context.AgentLabel())
	}
	fmt.Fprintf(&out, "[%d] %s/%s: %s\n", d.ID, d.IssueID, d.Stage, d.D.Question)
	if target := d.Review; target != nil {
		fmt.Fprintln(&out, "artifact review")
		fmt.Fprintf(&out, "    checkpoint: %d\n", target.CheckpointID)
		fmt.Fprintf(&out, "    artifact_version: %s\n", target.ArtifactVersion)
		if target.NextStage != "" {
			fmt.Fprintf(&out, "    next: %s\n", target.NextStage)
		}
		for _, artifact := range target.Artifacts {
			fmt.Fprintf(&out, "    %s: %s\n", artifact.Name, artifact.SHA256)
		}
	}
	if policy := d.ReviewPolicy; policy != nil {
		requirement := "human approval required"
		if policy.PolicyAutoApproval {
			requirement = "policy approval pending"
		}
		fmt.Fprintf(&out, "review policy: %s\n", requirement)
		fmt.Fprintf(&out, "    mode: %s\n", policy.Mode)
		fmt.Fprintf(&out, "    policy: %s@%s\n", policy.PolicyID, policy.PolicyVersion)
	}
	if d.D.Kind == levers.DecisionFreeform {
		fmt.Fprintf(&out, "    recommended: %s\n", d.D.RecommendedResponse)
		return out.String()
	}
	for i, option := range d.D.Options {
		mark := "  "
		if i == d.D.Recommended {
			mark = "* "
		}
		fmt.Fprintf(&out, "    %s%d) %s\n", mark, i, option)
	}
	if d.D.AllowFreeform {
		out.WriteString("      Other... (--text)\n")
	}
	return out.String()
}

// fakeForFlows builds a FakeRunner that succeeds every stage and writes
// every declared artifact — enough to exercise the pipeline end to end.
func fakeForFlows(flows map[string]flow.Flow) *runner.FakeRunner {
	scripts := map[string]runner.Script{}
	for _, f := range flows {
		for _, st := range f.Stages {
			arts := map[string]string{}
			var plannerRequests []plannerartifact.WriteRequest
			if st.DeclaresArtifact("plan.md") && st.DeclaresArtifact("touchset.json") {
				plannerRequests = fakePlannerRequests()
			}
			for _, a := range st.Artifacts {
				if plannerRequests != nil {
					continue
				}
				content := ""
				if a == "touchset.json" {
					content = `{"globs":["src/**"]}`
				}
				arts[a] = content
			}
			for _, ag := range st.Agents {
				script := runner.Script{
					Artifacts: arts, PlannerRequests: plannerRequests, Tokens: 10,
					SessionID: "fake-" + st.Name,
					Lines:     []string{fmt.Sprintf("fake %s/%s complete", st.Name, ag.Package)},
				}
				scripts[st.Name+"/"+ag.Package] = script
			}
		}
	}
	return &runner.FakeRunner{Scripts: scripts}
}

func fakePlannerRequests() []plannerartifact.WriteRequest {
	manifest := plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"src/gh40/goal/**"}},
		{Key: "architecture", Globs: []string{"src/gh40/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"src/gh40/technology/**"}},
		{Key: "execution-contract", Globs: []string{"src/gh40/contract/**"}},
		{Key: "file-structure", Globs: []string{"src/gh40/files/**"}},
		{Key: "task-0001", Globs: []string{"src/gh40/task-0001/**"}},
		{Key: "verification", Globs: []string{"src/gh40/verification/**"}},
	}}
	requests := make([]plannerartifact.WriteRequest, 0, len(manifest.Sections))
	for _, entry := range manifest.Sections {
		requests = append(requests, plannerartifact.WriteRequest{
			Manifest: manifest, Key: entry.Key, Markdown: "fake section " + entry.Key, Globs: entry.Globs,
		})
	}
	return requests
}

// attachFlag collects a repeatable --attach.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

func (a *attachFlag) Set(value string) error {
	*a = append(*a, value)
	return nil
}

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }

func (s *stringListFlag) Set(value string) error {
	*s = append(*s, value)
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
