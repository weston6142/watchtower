package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
)

const gatewayBodyLimit = 2 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

func (s *Session) startGateway() error {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return unsupported("gateway identity is unavailable")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return unsupported("gateway endpoint is unavailable")
	}
	path := "/mcp/" + hex.EncodeToString(tokenBytes)
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.serveMCP)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	s.gatewayListener = listener
	s.gatewayServer = server
	s.gatewayEndpoint = "http://" + listener.Addr().String() + path
	go func() { _ = server.Serve(listener) }()
	return nil
}

// GatewayEndpoint is an opaque, session-scoped endpoint for provider MCP
// configuration. It is authority only for the operations already present in
// the immutable contract and must not be persisted or logged.
func (s *Session) GatewayEndpoint() string {
	if s == nil {
		return ""
	}
	return s.gatewayEndpoint
}

func (s *Session) stopGateway() error {
	if s == nil || s.gatewayServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return s.gatewayServer.Shutdown(ctx)
}

func (s *Session) serveMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.RemoteAddr == "" {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		http.Error(writer, "forbidden", http.StatusForbidden)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, gatewayBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var call rpcRequest
	if err := decoder.Decode(&call); err != nil || call.JSONRPC != "2.0" || call.Method == "" {
		s.writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: call.ID, Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		s.writeRPC(writer, rpcResponse{JSONRPC: "2.0", ID: call.ID, Error: &rpcError{Code: -32600, Message: "invalid request"}})
		return
	}
	if len(call.ID) == 0 {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	response := rpcResponse{JSONRPC: "2.0", ID: call.ID}
	switch call.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "watchtower", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": s.mcpTools()}
	case "tools/call":
		result, dispatchErr := s.callMCP(request.Context(), call.Params)
		if dispatchErr != nil {
			response.Result = mcpToolResult{Content: []mcpContent{{Type: "text", Text: dispatchErr.Error()}}, IsError: true}
		} else {
			response.Result = mcpToolResult{Content: []mcpContent{{Type: "text", Text: result}}}
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	s.writeRPC(writer, response)
}

func (s *Session) writeRPC(writer http.ResponseWriter, response rpcResponse) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func (s *Session) mcpTools() []mcpTool {
	tools := make([]mcpTool, 0, len(s.contract.Contract.Operations))
	for _, operation := range s.contract.Contract.Operations {
		name := strings.ReplaceAll(string(operation), "-", "_")
		tool := mcpTool{Name: name, Description: "Watchtower-mediated " + string(operation)}
		switch operation {
		case capability.OpWorkspaceRead:
			tool.InputSchema = objectSchema(map[string]any{"path": stringSchema()}, "path")
		case capability.OpWorkspaceMutate:
			tool.InputSchema = objectSchema(map[string]any{
				"action": stringEnum("write", "remove", "rename", "chmod", "symlink"),
				"path":   stringSchema(), "from": stringSchema(), "to": stringSchema(), "target": stringSchema(),
				"content": stringSchema(), "mutation": stringEnum("create", "modify"), "mode": map[string]any{"type": "integer"},
			}, "action")
		case capability.OpLocalProcess:
			tool.InputSchema = objectSchema(map[string]any{"argv": map[string]any{"type": "array", "items": stringSchema(), "minItems": 1}}, "argv")
		case capability.OpVCSRead:
			tool.InputSchema = objectSchema(map[string]any{"args": map[string]any{"type": "array", "items": stringSchema(), "minItems": 1}}, "args")
		case capability.OpVCSCommit:
			tool.InputSchema = objectSchema(map[string]any{"message": stringSchema()}, "message")
		case capability.OpPlannerArtifactApply:
			tool.InputSchema = objectSchema(map[string]any{"artifact": map[string]any{}}, "artifact")
		default:
			continue
		}
		tools = append(tools, tool)
	}
	return tools
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }

func stringEnum(values ...string) map[string]any {
	items := make([]any, len(values))
	for index, value := range values {
		items[index] = value
	}
	return map[string]any{"type": "string", "enum": items}
}

func (s *Session) callMCP(ctx context.Context, raw json.RawMessage) (string, error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeStrict(raw, &call); err != nil || call.Name == "" {
		return "", s.deny("")
	}
	operation := capability.OperationClass(strings.ReplaceAll(call.Name, "_", "-"))
	if !s.hasOperation(operation) || strings.ReplaceAll(string(operation), "-", "_") != call.Name {
		return "", s.deny(operation)
	}
	switch operation {
	case capability.OpWorkspaceRead:
		var arguments struct {
			Path string `json:"path"`
		}
		if err := decodeStrict(call.Arguments, &arguments); err != nil || arguments.Path == "" {
			return "", s.deny(operation)
		}
		body, err := s.ReadFile(arguments.Path)
		if len(body) > maxOperationOutputBytes {
			return "", s.deny(operation, arguments.Path)
		}
		return string(body), err
	case capability.OpWorkspaceMutate:
		return s.callMutation(call.Arguments)
	case capability.OpLocalProcess:
		var arguments struct {
			Argv []string `json:"argv"`
		}
		if err := decodeStrict(call.Arguments, &arguments); err != nil {
			return "", s.deny(operation)
		}
		body, err := s.Run(ctx, arguments.Argv)
		return string(body), err
	case capability.OpVCSRead:
		var arguments struct {
			Args []string `json:"args"`
		}
		if err := decodeStrict(call.Arguments, &arguments); err != nil {
			return "", s.deny(operation)
		}
		body, err := s.VCSRead(ctx, arguments.Args...)
		return string(body), err
	case capability.OpVCSCommit:
		var arguments struct {
			Message string `json:"message"`
		}
		if err := decodeStrict(call.Arguments, &arguments); err != nil {
			return "", s.deny(operation)
		}
		return s.Commit(ctx, arguments.Message)
	case capability.OpPlannerArtifactApply:
		var arguments struct {
			Artifact any `json:"artifact"`
		}
		if err := decodeStrict(call.Arguments, &arguments); err != nil {
			return "", s.deny(operation)
		}
		return "applied", s.PlannerApply(arguments.Artifact)
	default:
		return "", s.deny(operation)
	}
}

func (s *Session) callMutation(raw json.RawMessage) (string, error) {
	var arguments struct {
		Action   string `json:"action"`
		Path     string `json:"path,omitempty"`
		From     string `json:"from,omitempty"`
		To       string `json:"to,omitempty"`
		Target   string `json:"target,omitempty"`
		Content  string `json:"content,omitempty"`
		Mutation string `json:"mutation,omitempty"`
		Mode     uint32 `json:"mode,omitempty"`
	}
	if err := decodeStrict(raw, &arguments); err != nil {
		return "", s.deny(capability.OpWorkspaceMutate)
	}
	var err error
	switch arguments.Action {
	case "write":
		mutation := capability.MutationClass(arguments.Mutation)
		if mutation != capability.MutationCreate && mutation != capability.MutationModify {
			return "", s.deny(capability.OpWorkspaceMutate, arguments.Path)
		}
		err = s.WriteFile(arguments.Path, []byte(arguments.Content), mutation)
	case "remove":
		err = s.Remove(arguments.Path)
	case "rename":
		err = s.Rename(arguments.From, arguments.To)
	case "chmod":
		err = s.Chmod(arguments.Path, os.FileMode(arguments.Mode))
	case "symlink":
		err = s.Symlink(arguments.Target, arguments.Path)
	default:
		return "", s.deny(capability.OpWorkspaceMutate, arguments.Path)
	}
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func decodeStrict(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}
