package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/repocfg"
)

func runPlannerArtifact(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "planner-artifact" || args[1] != "apply" {
		return fmt.Errorf("usage: watchtower planner-artifact apply [--request-file path]")
	}
	fs := flag.NewFlagSet("planner-artifact apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	requestPath := fs.String("request-file", "", "private request file")
	dataDir := fs.String("data", defaultData(), "base data dir")
	repoFlag := fs.String("repo", "", "target repo")
	socketPath := fs.String("socket", "", "existing daemon socket")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("request body must be supplied by file or standard input")
	}
	var reader io.Reader = stdin
	var requestFile *os.File
	if *requestPath != "" {
		var err error
		requestFile, err = os.Open(*requestPath)
		if err != nil {
			return fmt.Errorf("read planner request: %w", err)
		}
		defer requestFile.Close()
		reader = requestFile
	}
	const maxPlannerRequestBytes = plannerartifact.MaxOperationBytes * 2
	requestBytes, err := io.ReadAll(io.LimitReader(reader, maxPlannerRequestBytes+1))
	if err != nil || len(requestBytes) > maxPlannerRequestBytes {
		return fmt.Errorf("decode planner request: oversized request")
	}
	var request plannerartifact.WriteRequest
	decoder := json.NewDecoder(bytes.NewReader(requestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode planner request: malformed request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("decode planner request: trailing data")
	}
	if *socketPath == "" {
		repo := resolveRepo(*repoFlag)
		*socketPath = filepath.Join(repocfg.RepoDataDir(*dataDir, repo), sockFileName)
	}
	client, err := proto.Dial(*socketPath)
	if err != nil {
		return plannerartifact.NewAuthorityError(plannerartifact.ErrorTransportUnavailable, "", "daemon is unavailable")
	}
	defer client.Close()
	worktree, err := os.Getwd()
	if err != nil {
		return plannerartifact.NewAuthorityError(plannerartifact.ErrorTransportUnavailable, "", "worktree is unavailable")
	}
	issued, err := client.Do(proto.Command{Op: "planner_authority_issue", Worktree: worktree})
	if err != nil {
		return plannerartifact.NewAuthorityError(plannerartifact.ErrorTransportUnavailable, "", "daemon is unavailable")
	}
	if !issued.OK {
		return plannerResponseError(issued)
	}
	if issued.PlannerHandle == "" {
		return plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityState, request.Key, "daemon returned no capability")
	}
	result, err := client.Do(proto.Command{
		Op: "apply_planner_artifact", Worktree: worktree, PlannerHandle: issued.PlannerHandle,
		PlannerRequest: &request,
	})
	if err != nil {
		return plannerartifact.NewAuthorityError(plannerartifact.ErrorTransportUnavailable, request.Key, "daemon is unavailable")
	}
	if !result.OK {
		return plannerResponseError(result)
	}
	_, _ = fmt.Fprintf(stdout, "section-validated %s\n", request.Key)
	return nil
}

func plannerResponseError(response proto.Response) error {
	if response.Error != "" {
		return fmt.Errorf("%s", response.Error)
	}
	class := response.ErrorClass
	if class == "" {
		class = string(plannerartifact.ErrorAuthorityState)
	}
	return fmt.Errorf("planner authority: %s", class)
}
