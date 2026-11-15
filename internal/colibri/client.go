// Package colibri talks to a colibri server (github.com/JustVugg/colibri)
// through its OpenAI-compatible HTTP API.
package colibri

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
	"strings"
	"sync"
	"time"
)

// Client is a colibri API client. The zero value is not usable: set BaseURL.
type Client struct {
	// BaseURL is the server address as the user knows it, with or without
	// the /v1 suffix: both http://127.0.0.1:8000/ and http://127.0.0.1:8000/v1 work.
	BaseURL string
	// APIKey is sent as a Bearer token when not empty (COLI_API_KEY on the server).
	APIKey string
	// IdleTimeout aborts a stream that delivers no bytes for this long.
	// Colibri sends a keepalive every 10 seconds, so silence means a dead server.
	IdleTimeout time.Duration
	// HTTP is the client to use; http.DefaultTransport semantics when nil.
	HTTP *http.Client
}

// Model is one entry of GET /v1/models.
type Model struct {
	ID string
	// ContextLength is the context window in tokens, 0 when the server does not say.
	ContextLength int
}

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is what a generation needs.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Temperature float64
	// MaxTokens caps the reply; 0 leaves the server's own ceiling.
	MaxTokens int
}

// Delta is one streamed piece of a reply.
type Delta struct {
	// Content is answer text; Reasoning is the model's thinking channel.
	Content   string
	Reasoning string
}

// Usage is the token accounting of a reply, zero when not reported.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Result is a finished reply.
type Result struct {
	Content      string
	Reasoning    string
	FinishReason string
	Usage        Usage
}

// APIError is an error answer from the server.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch {
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return fmt.Sprintf("Колибри отклонил запрос (%d): проверьте API-ключ в настройках. %s", e.Status, msg)
	case e.Code == "context_length_exceeded":
		return "запрос не помещается в контекст модели: " + msg
	default:
		return fmt.Sprintf("Колибри ответил ошибкой %d: %s", e.Status, msg)
	}
}

// ContextExceeded reports whether err says the prompt does not fit the model's context.
func ContextExceeded(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == "context_length_exceeded"
}

// ErrIdle is returned when a stream stays silent longer than Client.IdleTimeout.
var ErrIdle = errors.New("Колибри перестал отвечать: поток молчит дольше допустимого")

func (c *Client) endpoint(path string) string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + path
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, fmt.Errorf("неверный адрес Колибри %q: %w", c.BaseURL, err)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// friendly turns transport failures into something a person can act on.
func (c *Client) friendly(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("нет связи с Колибри по адресу %s — запущен ли сервер? (%w)", c.BaseURL, err)
	}
	return err
}

func readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	apiErr := &APIError{Status: resp.StatusCode}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error.Message != "" {
		apiErr.Message = payload.Error.Message
		apiErr.Code = payload.Error.Code
		if apiErr.Code == "" {
			apiErr.Code = payload.Error.Type
		}
	} else {
		apiErr.Message = strings.TrimSpace(string(bytes.TrimSpace(data)))
		if len(apiErr.Message) > 300 {
			apiErr.Message = apiErr.Message[:300] + "…"
		}
	}
	return apiErr
}

// Models lists the models the server offers.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, c.friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			MaxModelLen   int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("по адресу %s отвечает не Колибри: список моделей не разобран (%w)", c.BaseURL, err)
	}
	models := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		ctxLen := m.ContextLength
		if ctxLen == 0 {
			ctxLen = m.MaxModelLen
		}
		models = append(models, Model{ID: m.ID, ContextLength: ctxLen})
	}
	return models, nil
}

type chatPayload struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"delta"`
		// Non-streaming servers answer with a whole message instead of deltas.
		Message *struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ChatStream sends a chat request and streams the reply. onDelta, when not
// nil, is called for every piece as it arrives, from the calling goroutine.
// On a mid-stream failure the partial Result is returned together with the error.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (Result, error) {
	body, err := json.Marshal(chatPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      true,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	if err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := newWatchdog(c.IdleTimeout, cancel)
	defer watch.stop()

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		if watch.fired() {
			return Result{}, ErrIdle
		}
		return Result{}, c.friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, readAPIError(resp)
	}

	var res Result
	var content, reasoning strings.Builder
	finish := func() Result {
		res.Content = content.String()
		res.Reasoning = reasoning.String()
		return res
	}
	apply := func(chunk *chatChunk) error {
		if chunk.Error != nil {
			code := chunk.Error.Code
			if code == "" {
				code = chunk.Error.Type
			}
			return &APIError{Status: http.StatusOK, Code: code, Message: chunk.Error.Message}
		}
		if chunk.Usage != nil {
			res.Usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			d := Delta{Content: ch.Delta.Content, Reasoning: ch.Delta.ReasoningContent}
			if d.Reasoning == "" {
				d.Reasoning = ch.Delta.Reasoning
			}
			if ch.Message != nil {
				d.Content += ch.Message.Content
				d.Reasoning += ch.Message.ReasoningContent
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				res.FinishReason = *ch.FinishReason
			}
			if d.Content == "" && d.Reasoning == "" {
				continue
			}
			content.WriteString(d.Content)
			reasoning.WriteString(d.Reasoning)
			if onDelta != nil {
				onDelta(d)
			}
		}
		return nil
	}

	// A server that ignores "stream" answers with one JSON document.
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		data, err := io.ReadAll(&touchReader{r: resp.Body, w: watch})
		if err != nil {
			if watch.fired() {
				return finish(), ErrIdle
			}
			return finish(), c.friendly(err)
		}
		var chunk chatChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return finish(), fmt.Errorf("ответ Колибри не разобран: %w", err)
		}
		if err := apply(&chunk); err != nil {
			return finish(), err
		}
		return finish(), nil
	}

	reader := bufio.NewReaderSize(&touchReader{r: resp.Body, w: watch}, 64<<10)
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				return finish(), nil
			}
			if data != "" {
				var chunk chatChunk
				if err := json.Unmarshal([]byte(data), &chunk); err == nil {
					if err := apply(&chunk); err != nil {
						return finish(), err
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if res.FinishReason == "" && content.Len() == 0 {
					return finish(), errors.New("Колибри закрыл соединение, не прислав ответ")
				}
				return finish(), nil
			}
			if watch.fired() {
				return finish(), ErrIdle
			}
			if ctx.Err() != nil {
				return finish(), ctx.Err()
			}
			return finish(), c.friendly(readErr)
		}
	}
}

// watchdog cancels a request when nothing has been read for the timeout.
type watchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	expired bool
}

func newWatchdog(timeout time.Duration, cancel context.CancelFunc) *watchdog {
	w := &watchdog{timeout: timeout}
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			w.mu.Lock()
			w.expired = true
			w.mu.Unlock()
			cancel()
		})
	}
	return w
}

func (w *watchdog) touch() {
	if w.timer != nil {
		w.timer.Reset(w.timeout)
	}
}

func (w *watchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *watchdog) fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expired
}

type touchReader struct {
	r io.Reader
	w *watchdog
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.w.touch()
	}
	return n, err
}
