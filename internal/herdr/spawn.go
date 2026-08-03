package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// SpawnPane splits a new pane beside the TUI's pane, focused, rooted at cwd,
// and types command into it. Unlike agent-state reporting, failures surface:
// the caller shows the operator a manual fallback.
func (r *Reporter) SpawnPane(cwd, command string) error {
	if !r.enabled() {
		return fmt.Errorf("not running under herdr")
	}
	res, err := r.call("pane.split", map[string]any{
		"direction":      "right",
		"cwd":            cwd,
		"focus":          true,
		"target_pane_id": r.paneID,
	})
	if err != nil {
		return fmt.Errorf("pane.split: %w", err)
	}
	paneID := paneIDFrom(res)
	if paneID == "" {
		return fmt.Errorf("pane.split: no pane_id in response")
	}
	if _, err := r.call("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    command + "\n",
	}); err != nil {
		return fmt.Errorf("pane.send_text: %w", err)
	}
	return nil
}

func paneIDFrom(result map[string]any) string {
	if id, ok := result["pane_id"].(string); ok && id != "" {
		return id
	}
	if pane, ok := result["pane"].(map[string]any); ok {
		if id, ok := pane["pane_id"].(string); ok {
			return id
		}
	}
	return ""
}

// call sends one NDJSON request and decodes the single response line.
func (r *Reporter) call(method string, params map[string]any) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", r.socketPath, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := map[string]any{
		"id":     fmt.Sprintf("watchtower:%d", time.Now().UnixNano()),
		"method": method,
		"params": params,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
	}
	if resp.Result == nil {
		resp.Result = map[string]any{}
	}
	return resp.Result, nil
}
