package marshal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

type Verification struct {
	BaseSHA   string     `json:"base_sha"`
	BranchSHA string     `json:"branch_sha"`
	TreeSHA   string     `json:"tree_sha"`
	Passed    bool       `json:"passed"`
	Commands  [][]string `json:"commands"`
}

func LoadVerification(path string) (Verification, error) {
	file, err := os.Open(path)
	if err != nil {
		return Verification{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var verification Verification
	if err := decoder.Decode(&verification); err != nil {
		return Verification{}, fmt.Errorf("decode verification: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Verification{}, err
	}
	if err := verification.Validate(); err != nil {
		return Verification{}, err
	}
	return verification, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode verification: trailing JSON value")
		}
		return fmt.Errorf("decode verification: %w", err)
	}
	return nil
}

func (v Verification) Validate() error {
	if strings.TrimSpace(v.BaseSHA) == "" ||
		strings.TrimSpace(v.BranchSHA) == "" ||
		strings.TrimSpace(v.TreeSHA) == "" {
		return fmt.Errorf("verification commit and tree SHAs are required")
	}
	if !v.Passed {
		return fmt.Errorf("verification did not pass")
	}
	return validateCommands(v.Commands)
}

func validateCommands(commands [][]string) error {
	if len(commands) == 0 {
		return fmt.Errorf("verification commands are required")
	}
	for commandIndex, argv := range commands {
		if len(argv) == 0 {
			return fmt.Errorf("verification command %d has empty argv", commandIndex)
		}
		for argumentIndex, argument := range argv {
			if strings.TrimSpace(argument) == "" {
				return fmt.Errorf(
					"verification command %d argument %d is empty",
					commandIndex, argumentIndex)
			}
		}
	}
	return nil
}

func (v Verification) AppliesTo(treeSHA string) bool {
	return v.Passed && v.TreeSHA != "" && v.TreeSHA == treeSHA
}

func (v Verification) Includes(required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, command := range v.Commands {
		if slices.Equal(command, required) {
			return true
		}
	}
	return false
}

func Replay(ctx context.Context, dir string, commands [][]string) error {
	if err := validateCommands(commands); err != nil {
		return err
	}
	for _, argv := range commands {
		command := exec.CommandContext(ctx, argv[0], argv[1:]...)
		command.Dir = dir
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf(
				"verification command %q failed: %v: %s",
				strings.Join(argv, " "), err, truncate(string(output), maxTestOutputBytes))
		}
	}
	return nil
}
