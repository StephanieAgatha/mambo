package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// StdioConfig holds the configuration to launch an MCP server subprocess.
type StdioConfig struct {
	Command string   // "uvx"
	Args    []string // ["--from", "tradingview-mcp-server", "tradingview-mcp"]
	Env     []string // extra env vars in "KEY=VALUE" format
}

// Client communicates with an MCP server via JSON-RPC 2.0 over stdin/stdout.
type Client struct {
	cfg         StdioConfig
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Scanner
	mu          sync.Mutex
	reqID       int
	CallTimeout time.Duration
}

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolResultContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolResultContent `json:"content"`
	IsError bool                `json:"isError"`
}

// NewClient spawns the MCP subprocess and performs the initialize handshake.
func NewClient(cfg StdioConfig) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(cmd.Environ(), cfg.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}

	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start subprocess %s: %w", cfg.Command, err)
	}

	c := &Client{
		cfg:         cfg,
		cmd:         cmd,
		stdin:       stdin,
		stdout:      bufio.NewScanner(stdoutPipe),
		CallTimeout: 30 * time.Second,
	}

	// MCP initialize handshake
	c.mu.Lock()
	c.reqID++
	c.reqID++
	initReq := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.reqID,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mambo-v2",
				"version": "2.0.0",
			},
		},
	}
	initReqBytes, err := json.Marshal(initReq)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: marshal initialize request: %w", err)
	}
	if _, err := io.WriteString(c.stdin, string(initReqBytes)+"\n"); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: write initialize request: %w", err)
	}

	// Read initialize response
	var initResp rpcResponse
	if !c.stdout.Scan() {
		c.Close()
		return nil, fmt.Errorf("mcp: no initialize response from subprocess")
	}
	if err := json.Unmarshal(c.stdout.Bytes(), &initResp); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: unmarshal initialize response: %w", err)
	}
	if initResp.Error != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: initialize error code=%d: %s", initResp.Error.Code, initResp.Error.Message)
	}

	// Send initialized notification
	c.reqID++
	notify := rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	notifyBytes, _ := json.Marshal(notify)
	if _, err := io.WriteString(c.stdin, string(notifyBytes)+"\n"); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: write initialized notification: %w", err)
	}

	c.mu.Unlock()

	slog.Info("mcp: client initialized",
		"command", cfg.Command,
		"args", cfg.Args,
	)

	return c, nil
}

// CallTool sends a tools/call JSON-RPC request and returns the result text.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reqID++
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.reqID,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": args,
		},
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	if _, err := io.WriteString(c.stdin, string(reqBytes)+"\n"); err != nil {
		return nil, fmt.Errorf("mcp: write request: %w", err)
	}

	type readResult struct {
		resp rpcResponse
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		if !c.stdout.Scan() {
			ch <- readResult{err: fmt.Errorf("mcp: no response from subprocess (subprocess may have crashed)")}
			return
		}
		var resp rpcResponse
		if err := json.Unmarshal(c.stdout.Bytes(), &resp); err != nil {
			ch <- readResult{err: fmt.Errorf("mcp: unmarshal response: %w", err)}
			return
		}
		ch <- readResult{resp: resp}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if r.resp.Error != nil {
			return nil, fmt.Errorf("mcp: rpc error code=%d: %s", r.resp.Error.Code, r.resp.Error.Message)
		}
		return r.resp.Result, nil
	}
}

// Close sends SIGTERM to the subprocess and waits for it to exit.
func (c *Client) Close() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}

	_ = c.cmd.Process.Kill()

	_ = c.cmd.Wait()
	return nil
}
