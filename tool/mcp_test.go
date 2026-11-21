package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMCPListAndCall(t *testing.T) {
	t.Setenv("ADA_TEST_MCP_API_KEY", "unused fixture value")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reg := new(Registry)
	t.Cleanup(func() { _ = reg.Close() })
	cfg := MCPConfig{
		Name: "sample", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
		Env: map[string]string{"ADA_TEST_MCP_HELPER": "1"}, Effect: Read,
	}
	if err := reg.AddMCP(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	specs := reg.Specs()
	if len(specs) != 2 || specs[0].Name != "sample__echo" || specs[1].Name != "sample__other" ||
		specs[0].Description != "Echo input." || string(specs[0].InputSchema) != `{"type":"object"}` ||
		string(specs[1].InputSchema) != `{"type":"object"}` {
		t.Fatalf("unexpected specs: %+v", specs)
	}
	if got, err := reg.Run(ctx, "sample__echo", json.RawMessage(`{"value":"hello"}`)); err != nil || got != "hello world" {
		t.Fatalf("Run = %q, %v", got, err)
	}
	if _, err := reg.Run(ctx, "sample__other", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "call failed") {
		t.Fatalf("Run = %v", err)
	}
}

func TestMCPToolNamesAndCollisions(t *testing.T) {
	for _, scenario := range []string{"names", "collision", "truncated-collision"} {
		t.Run(scenario, func(t *testing.T) {
			reg := new(Registry)
			t.Cleanup(func() { _ = reg.Close() })
			cfg := MCPConfig{
				Name: "sample.server", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
				Env: map[string]string{"ADA_TEST_MCP_HELPER": scenario}, Effect: Read,
			}
			err := reg.AddMCP(t.Context(), cfg)
			if scenario != "names" {
				if err == nil || !strings.Contains(err.Error(), "duplicate tool") || len(reg.Specs()) != 0 {
					t.Fatalf("AddMCP = %v, registered %v", err, reg.Specs())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			specs := reg.Specs()
			if len(specs) != 2 || specs[0].Name != "sample_server__echo_tools-_" ||
				specs[1].Name != "sample_server__"+strings.Repeat("x", 49) {
				t.Fatalf("specs = %+v", specs)
			}
			if got, err := reg.Run(t.Context(), specs[0].Name, json.RawMessage(`{"value":"hello"}`)); err != nil || got != "hello world" {
				t.Fatalf("Run = %q, %v", got, err)
			}
		})
	}
}

func TestMCPRestartsAfterCanceledCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reg := new(Registry)
	t.Cleanup(func() { _ = reg.Close() })
	cfg := MCPConfig{
		Name: "sample", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
		Env: map[string]string{"ADA_TEST_MCP_HELPER": "1"}, Effect: Read,
	}
	if err := reg.AddMCP(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelCall()
	if _, err := reg.Run(callCtx, "sample__echo", json.RawMessage(`{"value":"hang"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled Run = %v", err)
	}
	if got, err := reg.Run(ctx, "sample__echo", json.RawMessage(`{"value":"hello"}`)); err != nil || got != "hello world" {
		t.Fatalf("Run after restart = %q, %v", got, err)
	}
}

func TestMCPRejectsEffects(t *testing.T) {
	for _, effect := range []Effect{DelegateEffect, Send, Account, ""} {
		err := new(Registry).AddMCP(t.Context(), MCPConfig{Name: "blocked", Command: "must not run", Effect: effect})
		if err == nil || !strings.Contains(err.Error(), "must declare effect read or write") {
			t.Fatalf("AddMCP = %v", err)
		}
	}
}

func TestMCPHelperProcess(t *testing.T) {
	scenario := os.Getenv("ADA_TEST_MCP_HELPER")
	if scenario == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	echo, other := "echo", "other"
	switch scenario {
	case "names":
		echo, other = "echo.tools-\u00e9", strings.Repeat("x", 70)
	case "collision":
		echo, other = "echo.tools", "echo_tools"
	case "truncated-collision":
		echo, other = strings.Repeat("x", 64)+"a", strings.Repeat("x", 64)+"b"
	}
	for step := 0; scanner.Scan(); step++ {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.JSONRPC != "2.0" {
			os.Exit(2)
		}
		if step == 2 && req.Method == "tools/call" {
			step = 4
		}
		var result string
		switch step {
		case 0:
			info, _ := req.Params["clientInfo"].(map[string]any)
			capabilities, ok := req.Params["capabilities"].(map[string]any)
			if req.Method != "initialize" || req.Params["protocolVersion"] != "2025-06-18" || info["name"] != "adanext-ada" || !ok || len(capabilities) != 0 {
				os.Exit(2)
			}
			result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"}}`
		case 1:
			if req.Method != "notifications/initialized" || len(req.ID) != 0 {
				os.Exit(2)
			}
			continue
		case 2:
			if req.Method != "tools/list" || req.Params["cursor"] != nil {
				os.Exit(2)
			}
			result = fmt.Sprintf(`{"tools":[{"name":%q,"description":"Echo input."}],"nextCursor":"page-2"}`, echo)
		case 3:
			if req.Method != "tools/list" || req.Params["cursor"] != "page-2" {
				os.Exit(2)
			}
			result = fmt.Sprintf(`{"tools":[{"name":%q,"inputSchema":null}]}`, other)
		case 4:
			args, _ := req.Params["arguments"].(map[string]any)
			if req.Method != "tools/call" || req.Params["name"] != echo {
				os.Exit(2)
			}
			if args["value"] == "hang" {
				time.Sleep(30 * time.Second)
				os.Exit(2)
			}
			if args["value"] != "hello" {
				os.Exit(2)
			}
			if scenario == "1" && !mcpHelperCheckRequests(scanner, encoder) {
				os.Exit(2)
			}
			result = `{"content":[{"type":"text","text":"hello "},{"type":"image","data":"ignored"},{"type":"text","text":"world"}]}`
		case 5:
			if req.Method != "tools/call" || req.Params["name"] != "other" {
				os.Exit(2)
			}
			result = `{"isError":true,"content":[{"type":"text","text":"call failed"}]}`
		default:
			fmt.Fprintln(os.Stderr, "unexpected MCP message")
			os.Exit(2)
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(result)}); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func mcpHelperCheckRequests(scanner *bufio.Scanner, encoder *json.Encoder) bool {
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message"}); err != nil {
		return false
	}
	for _, req := range []struct{ method, id string }{{"ping", `0`}, {"roots/list", `"server-request"`}} {
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": req.method, "id": json.RawMessage(req.id)}); err != nil || !scanner.Scan() {
			return false
		}
		var reply struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   struct{ Code int }
		}
		if json.Unmarshal(scanner.Bytes(), &reply) != nil || reply.JSONRPC != "2.0" || string(reply.ID) != req.id ||
			len(reply.Result) != 0 || reply.Error.Code != -32601 {
			return false
		}
	}
	_, leaked := os.LookupEnv("ADA_TEST_MCP_API_KEY")
	return !leaked
}
