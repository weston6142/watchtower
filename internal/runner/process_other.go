//go:build !unix

package runner

import "fmt"

func startProcessTree(ProcessSpec) (*ProcessTree, error) {
	return nil, fmt.Errorf("process-tree containment is unsupported on this platform")
}
