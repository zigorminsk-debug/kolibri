package lightpanda

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	mcpProtocolVersion = "2025-11-25"
	maxMCPLineBytes    = 16 << 20
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("ошибка MCP (%d)", e.Code)
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type toolOutput struct {
	Text       string
	PageURL    string
	Title      string
	HTTPStatus int
	IsError    bool
}

type callToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured *struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		HTTPStatus int    `json:"httpStatus"`
	} `json:"structuredContent"`
	IsError bool `json:"isError"`
}

// mcpConn is the wire transport underneath an MCP session (stdio or HTTP).
type mcpConn interface {
	initialize(ctx context.Context) (initResult, error)
	callTool(ctx context.Context, name string, args map[string]any) (toolOutput, error)
	close() error
}

func decodeToolResult(raw json.RawMessage) (toolOutput, error) {
	var res callToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return toolOutput{}, fmt.Errorf("ответ Lightpanda не разобран: %w", err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" && c.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(c.Text)
		}
	}
	out := toolOutput{Text: b.String(), IsError: res.IsError}
	if res.Structured != nil {
		out.PageURL = res.Structured.URL
		out.Title = res.Structured.Title
		out.HTTPStatus = res.Structured.HTTPStatus
	}
	return out, nil
}

func initPayload(id int64) rpcRequest {
	return rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]string{
				"name":    "kolibri-forge",
				"version": "1.0",
			},
		},
	}
}

func initializedNotification() rpcRequest {
	return rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
}

func cancelNotification(id int64) rpcRequest {
	return rpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  map[string]any{"requestId": id},
	}
}

// ---- stdio transport ----------------------------------------------------

// stdioConn drives a local Lightpanda process running `lightpanda mcp`
// (directly, inside WSL2, or inside Docker). If a cancelled call forces a
// restart, spawn makes a fresh process for the next action.
type stdioConn struct {
	spawn func() (*exec.Cmd, error)

	mu   sync.Mutex
	proc *stdioProc
	seq  atomic.Int64
	dead bool
}

type stdioProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *tailBuffer

	wmu     sync.Mutex
	pmu     sync.Mutex
	pending map[int64]chan rpcResponse
	done    chan struct{}
	readErr error
}

func newStdioConn(ctx context.Context, spawn func() (*exec.Cmd, error)) (*stdioConn, initResult, error) {
	c := &stdioConn{spawn: spawn}
	info, err := c.initialize(ctx)
	if err != nil {
		_ = c.close()
		return nil, initResult{}, err
	}
	return c, info, nil
}

func (c *stdioConn) startProc() (*stdioProc, error) {
	cmd, err := c.spawn()
	if err != nil {
		return nil, err
	}
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("не удалось запустить Lightpanda: %w", err)
	}
	p := &stdioProc{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  stderr,
		pending: map[int64]chan rpcResponse{},
		done:    make(chan struct{}),
	}
	go p.readLoop(stdout)
	return p, nil
}

func (p *stdioProc) readLoop(r io.Reader) {
	defer func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	var line bytes.Buffer
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if line.Len()+len(chunk) <= maxMCPLineBytes {
				line.Write(chunk)
			} else {
				// Oversized line: drain until the next newline.
				line.Reset()
				for errors.Is(err, bufio.ErrBufferFull) {
					_, err = br.ReadSlice('\n')
				}
				if err != nil {
					p.readErr = err
					return
				}
			}
			continue
		}
		var raw []byte
		if line.Len() > 0 {
			if line.Len()+len(chunk) <= maxMCPLineBytes {
				line.Write(chunk)
				raw = line.Bytes()
			}
			line.Reset()
		} else {
			raw = chunk
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && raw[0] == '{' {
			var resp rpcResponse
			if json.Unmarshal(raw, &resp) == nil && resp.ID != nil {
				p.pmu.Lock()
				ch := p.pending[*resp.ID]
				delete(p.pending, *resp.ID)
				p.pmu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
		}
		if err != nil {
			p.readErr = err
			return
		}
	}
}

func (p *stdioProc) writeMsg(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(data)
	return err
}

func (p *stdioProc) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, bool, error) {
	ch := make(chan rpcResponse, 1)
	p.pmu.Lock()
	p.pending[id] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
	}()

	if err := p.writeMsg(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, true, p.exitError("не удалось отправить команду Lightpanda", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, false, resp.Error
		}
		return resp.Result, false, nil
	case <-p.done:
		return nil, true, p.exitError("Lightpanda неожиданно завершился", p.readErr)
	case <-ctx.Done():
		// Ask Lightpanda to cancel the running call first; kill the process
		// only if it ignores cancellation.
		_ = p.writeMsg(cancelNotification(id))
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ch:
			return nil, false, ctx.Err()
		case <-p.done:
			return nil, true, ctx.Err()
		case <-timer.C:
			p.kill()
			return nil, true, ctx.Err()
		}
	}
}

func (p *stdioProc) exitError(prefix string, err error) error {
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		return fmt.Errorf("%s: %s", prefix, oneLine(tail, 280))
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return errors.New(prefix)
}

func (p *stdioProc) stop() {
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.kill()
	}
}

func (p *stdioProc) kill() {
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	}
}

func (c *stdioConn) ensureProc(ctx context.Context) (*stdioProc, initResult, error) {
	if c.dead {
		return nil, initResult{}, errors.New("сессия Lightpanda закрыта")
	}
	if c.proc != nil {
		select {
		case <-c.proc.done:
			c.proc = nil
		default:
			return c.proc, initResult{}, nil
		}
	}
	p, err := c.startProc()
	if err != nil {
		return nil, initResult{}, err
	}
	id := c.seq.Add(1)
	initReq := initPayload(id)
	raw, _, err := p.call(ctx, id, initReq.Method, initReq.Params)
	if err != nil {
		p.kill()
		return nil, initResult{}, err
	}
	var info initResult
	if err := json.Unmarshal(raw, &info); err != nil {
		p.kill()
		return nil, initResult{}, fmt.Errorf("Lightpanda прислал неожиданный ответ при запуске: %w", err)
	}
	_ = p.writeMsg(initializedNotification())
	c.proc = p
	return p, info, nil
}

func (c *stdioConn) initialize(ctx context.Context) (initResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, info, err := c.ensureProc(ctx)
	return info, err
}

func (c *stdioConn) callTool(ctx context.Context, name string, args map[string]any) (toolOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, _, err := c.ensureProc(ctx)
	if err != nil {
		return toolOutput{}, err
	}
	id := c.seq.Add(1)
	raw, drop, err := p.call(ctx, id, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if drop && c.proc == p {
		c.proc = nil
	}
	if err != nil {
		return toolOutput{}, err
	}
	return decodeToolResult(raw)
}

func (c *stdioConn) close() error {
	c.mu.Lock()
	c.dead = true
	p := c.proc
	c.proc = nil
	c.mu.Unlock()
	if p != nil {
		p.stop()
	}
	return nil
}

// tailBuffer keeps the last max bytes written to it (process stderr).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ---- Streamable HTTP transport ------------------------------------------

// httpConn drives a Lightpanda server started with `lightpanda mcp --port N`.
// Each connection holds its own Mcp-Session-Id and closes it on exit.
type httpConn struct {
	endpoint string
	display  string
	client   *http.Client

	mu        sync.Mutex
	sessionID string
	seq       atomic.Int64
}

var errSessionExpired = errors.New("MCP session expired")

// NormalizeMCPURL turns a user-entered address (http://127.0.0.1:9223 or
// http://127.0.0.1:9223/mcp) into the POST endpoint and rejects common
// mistakes such as pointing at a ws:// CDP server.
func NormalizeMCPURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("не указан адрес MCP-сервера Lightpanda")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("неверный адрес Lightpanda %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "ws" || scheme == "wss" {
		return "", fmt.Errorf("указан WebSocket-адрес CDP (%s), а нужен HTTP-сервер MCP: запустите «lightpanda mcp --host 127.0.0.1 --port 9223» и укажите http://127.0.0.1:9223", raw)
	}
	if scheme != "http" && scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("адрес MCP-сервера Lightpanda должен выглядеть как http://127.0.0.1:9223")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/mcp"
	}
	return u.String(), nil
}

func newHTTPConn(ctx context.Context, rawURL string, client *http.Client) (*httpConn, initResult, error) {
	endpoint, err := NormalizeMCPURL(rawURL)
	if err != nil {
		return nil, initResult{}, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := &httpConn{endpoint: endpoint, display: strings.TrimSpace(rawURL), client: client}
	info, err := c.initialize(ctx)
	if err != nil {
		return nil, initResult{}, err
	}
	return c, info, nil
}

func (c *httpConn) initialize(ctx context.Context) (initResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initLocked(ctx)
}

func (c *httpConn) initLocked(ctx context.Context) (initResult, error) {
	c.sessionID = ""
	id := c.seq.Add(1)
	raw, sid, err := c.postLocked(ctx, initPayload(id), true)
	if err != nil {
		return initResult{}, err
	}
	var info initResult
	if err := json.Unmarshal(raw, &info); err != nil {
		return initResult{}, fmt.Errorf("по адресу %s отвечает не MCP-сервер Lightpanda (%w)", c.display, err)
	}
	c.sessionID = sid
	_, _, _ = c.postLocked(ctx, initializedNotification(), false)
	return info, nil
}

func (c *httpConn) callTool(ctx context.Context, name string, args map[string]any) (toolOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if c.sessionID == "" && attempt > 0 {
			if _, err := c.initLocked(ctx); err != nil {
				return toolOutput{}, err
			}
		}
		id := c.seq.Add(1)
		raw, sid, err := c.postLocked(ctx, rpcRequest{
			JSONRPC: "2.0",
			ID:      &id,
			Method:  "tools/call",
			Params:  map[string]any{"name": name, "arguments": args},
		}, true)
		if errors.Is(err, errSessionExpired) && attempt == 0 {
			// A slow local model can pause long enough between turns for
			// --http-session-timeout to reap the session; start a fresh one.
			c.sessionID = ""
			continue
		}
		if err != nil {
			if ctx.Err() != nil && c.sessionID != "" {
				c.sendCancelLocked(id)
			}
			return toolOutput{}, err
		}
		if sid != "" && c.sessionID == "" {
			c.sessionID = sid
		}
		return decodeToolResult(raw)
	}
	return toolOutput{}, errSessionExpired
}

func (c *httpConn) postLocked(ctx context.Context, payload rpcRequest, expectReply bool) (json.RawMessage, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Lightpanda rejects browser Origin headers to block cross-site abuse.
	req.Header.Del("Origin")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		var netErr *net.OpError
		if errors.As(err, &netErr) {
			return nil, "", fmt.Errorf("нет связи с Lightpanda по адресу %s — запущен ли «lightpanda mcp --port …»? (%w)", c.display, err)
		}
		return nil, "", err
	}
	defer resp.Body.Close()

	sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxMCPLineBytes))
	if readErr != nil {
		if ctx.Err() != nil {
			return nil, sid, ctx.Err()
		}
		return nil, sid, readErr
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
	case http.StatusNotFound:
		if c.sessionID != "" {
			return nil, "", errSessionExpired
		}
		return nil, "", fmt.Errorf("по адресу %s отвечает не MCP-сервер Lightpanda (404). Запустите «lightpanda mcp --host 127.0.0.1 --port 9223»", c.display)
	case http.StatusForbidden:
		msg := strings.TrimSpace(string(data))
		if strings.Contains(strings.ToLower(msg), "host not allowed") {
			return nil, "", fmt.Errorf("Lightpanda отклонил имя хоста в %s: обращайтесь по IP-адресу (например http://127.0.0.1:9223)", c.display)
		}
		return nil, "", fmt.Errorf("Lightpanda запретил подключение (403): %s", oneLine(msg, 200))
	default:
		msg := oneLine(string(data), 200)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusUpgradeRequired {
			return nil, "", fmt.Errorf("по адресу %s отвечает не MCP-сервер Lightpanda (%d): убедитесь, что запущен «lightpanda mcp --port …», а не «lightpanda serve»", c.display, resp.StatusCode)
		}
		return nil, "", fmt.Errorf("Lightpanda ответил ошибкой %d: %s", resp.StatusCode, msg)
	}

	if !expectReply || len(bytes.TrimSpace(data)) == 0 {
		return nil, sid, nil
	}

	rpcResp, err := parseHTTPRPCResponse(data, resp.Header.Get("Content-Type"), payload.ID)
	if err != nil {
		return nil, sid, err
	}
	if rpcResp.Error != nil {
		return nil, sid, rpcResp.Error
	}
	return rpcResp.Result, sid, nil
}

// parseHTTPRPCResponse reads either a plain JSON reply or an SSE stream
// (event: message / data: {...}) and picks the frame answering wantID.
func parseHTTPRPCResponse(data []byte, contentType string, wantID *int64) (rpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "event-stream") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("event:")) || bytes.HasPrefix(bytes.TrimSpace(data), []byte(":")) {
		var last *rpcResponse
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			payload, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			payload = strings.TrimSpace(payload)
			if payload == "" {
				continue
			}
			var r rpcResponse
			if json.Unmarshal([]byte(payload), &r) != nil || r.ID == nil {
				continue
			}
			cp := r
			last = &cp
			if wantID != nil && *r.ID == *wantID {
				return r, nil
			}
		}
		if last != nil {
			return *last, nil
		}
		return rpcResponse{}, errors.New("пустой потоковый ответ от Lightpanda")
	}
	var r rpcResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return rpcResponse{}, fmt.Errorf("ответ Lightpanda не разобран: %w", err)
	}
	return r, nil
}

func (c *httpConn) sendCancelLocked(id int64) {
	body, err := json.Marshal(cancelNotification(id))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	resp, err := c.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func (c *httpConn) close() error {
	c.mu.Lock()
	sid := c.sessionID
	c.sessionID = ""
	c.mu.Unlock()
	if sid == "" || sid == "default" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := c.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return nil
}

func oneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes-1]) + "…"
	}
	return s
}
