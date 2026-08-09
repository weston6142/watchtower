package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/weston6142/watchtower/internal/plannerartifact"
)

func runPlannerArtifact(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "planner-artifact" || args[1] != "apply" {
		return fmt.Errorf("usage: watchtower planner-artifact apply [--request-file path]")
	}
	fs := flag.NewFlagSet("planner-artifact apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	requestPath := fs.String("request-file", "", "private request file")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("request body must be supplied by file or standard input")
	}
	var reader io.Reader = stdin
	var file *os.File
	if *requestPath != "" {
		var err error
		file, err = os.Open(*requestPath)
		if err != nil {
			return fmt.Errorf("read planner request: %w", err)
		}
		defer file.Close()
		reader = file
	}
	var request plannerartifact.WriteRequest
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode planner request: malformed request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("decode planner request: trailing data")
	}
	if err := plannerartifact.ApplyFromFD(3, request); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "section-validated %s\n", request.Key)
	return nil
}
