package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	serverName    = "svcdeck"
	serverVersion = "0.1.0"
)

// Server 是传输无关的 MCP Server：stdio 和 HTTP 都调用同一套 HandleJSON。
type Server struct {
	api *apiClient
}

// New 创建 MCP Server。Config 只需要面板地址与 token，不需要 -data/-db。
func New(cfg Config) (*Server, error) {
	api, err := newAPIClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{api: api}, nil
}

// Run 启动 stdio MCP Server。in/out 由根包传入 os.Stdin/os.Stdout。
// stdout 只输出 JSON-RPC；日志由调用方写 stderr。
func Run(cfg Config, in io.Reader, out io.Writer) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	return s.run(in, out)
}

// HandleJSON 处理一条 JSON-RPC 消息，返回可直接写回客户端的 JSON。
// ok=false 表示这是 notification，不需要响应。
func (s *Server) HandleJSON(ctx context.Context, raw []byte) (json.RawMessage, bool, error) {
	resp, ok := s.handleLineContext(ctx, raw)
	if !ok {
		return nil, false, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return nil, false, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), true, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) run(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		resp, ok, err := s.HandleJSON(context.Background(), []byte(line))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := w.Write(append(resp, '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handleLine(line []byte) (*rpcResponse, bool) {
	return s.handleLineContext(context.Background(), line)
}

func (s *Server) handleLineContext(ctx context.Context, line []byte) (*rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: -32700, Message: "Parse error"},
		}, true
	}
	if req.JSONRPC != "2.0" {
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32600, Message: "Invalid Request: jsonrpc must be \"2.0\""},
		}, true
	}

	// 通知没有 id，处理完不需要返回；初始化完成通知尤为重要。
	if len(req.ID) == 0 {
		return nil, false
	}

	result, rerr := s.dispatch(ctx, req)
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + req.Method}
	}
}

func (s *Server) initialize(raw json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &p)

	version := strings.TrimSpace(p.ProtocolVersion)
	if version == "" {
		version = "2024-11-05"
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
		"instructions": "SvcDeck 是 Windows 裸 exe 服务面板。新增服务用 add_service：exe 必须是面板所在机器上的绝对路径；enabled=false 只登记不启动，enabled=true 会立即拉起。update_service 是局部合并（先 GET 再 PUT）。stop_service / delete_service 是破坏性操作，必须先得到用户明确确认。",
	}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) any {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return toolError(err)
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	def, ok := findTool(p.Name)
	if !ok {
		return toolError(errors.New("未知工具: " + p.Name))
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
	}

	text, err := def.handler(ctx, s.api, p.Arguments)
	if err != nil {
		return toolError(err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "错误: " + err.Error()}},
		"isError": true,
	}
}
