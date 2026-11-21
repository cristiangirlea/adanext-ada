package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/cristiangirlea/adanext-ada/provider"
)

const mcpVersion = "2025-06-18"

type MCPConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Effect  Effect            `json:"effect"`
}

func (cfg MCPConfig) Validate() error {
	if cfg.Effect != Read && cfg.Effect != Write {
		return fmt.Errorf("MCP server %q must declare effect read or write", cfg.Name)
	}
	if cfg.Name == "" || cfg.Command == "" {
		return fmt.Errorf("MCP server requires a name and command")
	}
	return nil
}

type mcpClient struct {
	cfg    MCPConfig
	gate   chan struct{}
	mu     sync.Mutex
	proc   *mcpProcess
	closed bool
}

type mcpProcess struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	scanner   *bufio.Scanner
	closeOnce sync.Once
	closed    chan struct{}
	nextID    uint64
}

type mcpRequest struct {
	Version string `json:"jsonrpc"`
	ID      uint64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpReply struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (r *Registry) AddMCP(ctx context.Context, cfg MCPConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client := &mcpClient{cfg: cfg, gate: make(chan struct{}, 1)}
	keep := false
	defer func() {
		if !keep {
			_ = client.Close()
		}
	}()
	tools, err := client.listTools(ctx, cfg.Name)
	if err != nil {
		return fmt.Errorf("load MCP tools from %q: %w", cfg.Name, err)
	}
	pending := new(Registry)
	for _, t := range tools {
		if _, exists := r.tools[t.spec.Name]; exists {
			return fmt.Errorf("duplicate tool %q", t.spec.Name)
		}
		if err := pending.Add(t); err != nil {
			return err
		}
	}
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	maps.Copy(r.tools, pending.tools)
	r.closers = append(r.closers, client)
	keep = true
	return nil
}

var mcpInheritedEnv = []string{
	"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "windir", "ComSpec",
	"TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "LANG", "LC_ALL",
}

func startMCP(cfg MCPConfig) (*mcpProcess, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return !slices.ContainsFunc(mcpInheritedEnv, func(allowed string) bool {
			return name == allowed || runtime.GOOS == "windows" && strings.EqualFold(name, allowed)
		})
	})
	for key, value := range cfg.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32<<10), 8<<20)
	return &mcpProcess{cmd: cmd, stdin: stdin, stdout: stdout, scanner: scanner, closed: make(chan struct{})}, nil
}

func (c *mcpClient) Close() error {
	c.mu.Lock()
	c.closed = true
	p := c.proc
	c.mu.Unlock()
	if p != nil {
		p.close()
	}
	return nil
}

func (p *mcpProcess) close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		_ = p.cmd.Process.Kill()
		_ = p.stdout.Close()
		_ = p.stdin.Close()
		_ = p.cmd.Wait()
	})
}

func (c *mcpClient) rpc(ctx context.Context, method string, params, result any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := c.process(ctx)
	if err != nil {
		return err
	}
	return p.rpc(ctx, method, params, result)
}

func (c *mcpClient) process(ctx context.Context) (*mcpProcess, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("MCP server %q is closed", c.cfg.Name)
	}
	if p := c.proc; p != nil {
		select {
		case <-p.closed:
		default:
			c.mu.Unlock()
			return p, nil
		}
	}
	p, err := startMCP(c.cfg)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("start MCP server %q: %w", c.cfg.Name, err)
	}
	c.proc = p
	c.mu.Unlock()
	if err := p.initialize(ctx); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (p *mcpProcess) rpc(ctx context.Context, method string, params, result any) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A canceled exchange cannot leave a reply in the stream for the next request.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		p.close()
	})
	defer func() {
		if !stop() {
			<-done
			err = ctx.Err()
		}
	}()
	req := mcpRequest{Version: "2.0", Method: method, Params: params}
	if result != nil {
		p.nextID++
		req.ID = p.nextID
	}
	encoder := json.NewEncoder(p.stdin)
	if err := encoder.Encode(req); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send MCP %s: %w", method, err)
	}
	if result == nil {
		return ctx.Err()
	}
	for p.scanner.Scan() {
		var reply mcpReply
		if err := json.Unmarshal(p.scanner.Bytes(), &reply); err != nil {
			return fmt.Errorf("decode MCP %s response: %w", method, err)
		}
		if reply.Version != "2.0" {
			return fmt.Errorf("MCP %s response has an unexpected JSON-RPC version", method)
		}
		if reply.Method != nil {
			if len(reply.ID) != 0 {
				response := mcpReply{Version: "2.0", ID: reply.ID, Error: &mcpError{Code: -32601, Message: "Method not found"}}
				if err := encoder.Encode(response); err != nil {
					return fmt.Errorf("answer MCP server request %q: %w", *reply.Method, err)
				}
			}
			continue
		}
		var id uint64
		if err := json.Unmarshal(reply.ID, &id); err != nil || id != req.ID {
			return fmt.Errorf("MCP %s response has an unexpected ID", method)
		}
		if reply.Error != nil {
			return fmt.Errorf("MCP %s (%d): %s", method, reply.Error.Code, reply.Error.Message)
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("decode MCP %s result: %w", method, err)
		}
		return ctx.Err()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = p.scanner.Err()
	if err == nil {
		err = io.EOF
	}
	return fmt.Errorf("read MCP %s response: %w", method, err)
}

func (p *mcpProcess) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": mcpVersion,
		"capabilities":    struct{}{},
		"clientInfo":      map[string]string{"name": "adanext-ada", "version": "dev"},
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := p.rpc(ctx, "initialize", params, &initialized); err != nil {
		return fmt.Errorf("initialize MCP server: %w", err)
	}
	if initialized.ProtocolVersion != mcpVersion {
		return fmt.Errorf("unsupported MCP protocol version %q", initialized.ProtocolVersion)
	}
	return p.rpc(ctx, "notifications/initialized", nil, nil)
}

func (c *mcpClient) listTools(ctx context.Context, server string) ([]mcpTool, error) {
	var tools []mcpTool
	var cursor string
	seen := make(map[string]struct{})
	for {
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		params := struct {
			Cursor string `json:"cursor,omitempty"`
		}{cursor}
		if err := c.rpc(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tools {
			if t.Name == "" {
				return nil, fmt.Errorf("MCP tool name is required")
			}
			if len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
				t.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, mcpTool{client: c, name: t.Name, spec: provider.ToolSpec{
				Name: mcpToolName(server, t.Name), Description: t.Description, InputSchema: t.InputSchema,
			}})
		}
		cursor = page.NextCursor
		if cursor == "" {
			return tools, nil
		}
		if _, exists := seen[cursor]; exists {
			return nil, fmt.Errorf("MCP tools/list repeated a pagination cursor")
		}
		seen[cursor] = struct{}{}
	}
}

func mcpToolName(server, tool string) string {
	var name strings.Builder
	for _, ch := range server + "__" + tool {
		if !('A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' || ch == '_' || ch == '-') {
			ch = '_'
		}
		name.WriteByte(byte(ch))
		if name.Len() == 64 {
			break
		}
	}
	return name.String()
}

type mcpTool struct {
	client *mcpClient
	name   string
	spec   provider.ToolSpec
}

func (t mcpTool) Spec() provider.ToolSpec { return t.spec }
func (t mcpTool) Effect() Effect          { return t.client.cfg.Effect }

func (t mcpTool) Summary(input json.RawMessage) (string, error) {
	if err := decodeInput(input, new(json.RawMessage)); err != nil {
		return "", err
	}
	args, err := canonicalArgs(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("MCP server=%q tool=%q arguments=%s", t.client.cfg.Name, t.name, args), nil
}

func (t mcpTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args json.RawMessage
	if err := decodeInput(input, &args); err != nil {
		return "", err
	}
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{t.name, args}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := t.client.rpc(ctx, "tools/call", params, &result); err != nil {
		return "", err
	}
	var text strings.Builder
	for _, content := range result.Content {
		if content.Type == "text" {
			text.WriteString(content.Text)
		}
	}
	if result.IsError {
		return "", fmt.Errorf("MCP tool %q failed: %s", t.spec.Name, text.String())
	}
	return text.String(), nil
}
