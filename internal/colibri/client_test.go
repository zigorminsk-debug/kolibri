package colibri

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEndpointAcceptsEveryFormOfTheAddress(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:8000", "http://127.0.0.1:8000/", "http://127.0.0.1:8000/v1", " http://127.0.0.1:8000/v1/ "} {
		c := &Client{BaseURL: base}
		if got := c.endpoint("/models"); got != "http://127.0.0.1:8000/v1/models" {
			t.Errorf("endpoint(%q) = %q", base, got)
		}
	}
}

func TestModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"Invalid API key.","type":"invalid_api_key"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"a","context_length":65536},{"id":"b","max_model_len":4096},{"id":""}]}`)
	}))
	defer srv.Close()

	models, err := (&Client{BaseURL: srv.URL, APIKey: "secret"}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != (Model{"a", 65536}) || models[1] != (Model{"b", 4096}) {
		t.Errorf("models = %+v", models)
	}

	_, err = (&Client{BaseURL: srv.URL}).Models(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Status != 401 || !strings.Contains(err.Error(), "API-ключ") {
		t.Errorf("err = %v", err)
	}
}

func TestModelsAgainstSomethingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>router login</html>")
	}))
	defer srv.Close()
	if _, err := (&Client{BaseURL: srv.URL}).Models(context.Background()); err == nil || !strings.Contains(err.Error(), "не Колибри") {
		t.Errorf("err = %v", err)
	}
}

func stream(t *testing.T, handler http.HandlerFunc, idle time.Duration) (Result, []Delta, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	var deltas []Delta
	res, err := (&Client{BaseURL: srv.URL, IdleTimeout: idle}).ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d Delta) { deltas = append(deltas, d) })
	return res, deltas, err
}

func TestChatStream(t *testing.T) {
	res, deltas, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\".\"},\"finish_reason\":null}]}\r\n\r\n")
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "data: {\"compaction\":{\"summarized_messages\":2},\"choices\":[]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"При\"}}]}\n\n")
		fmt.Fprint(w, "data:{\"choices\":[{\"delta\":{\"content\":\"вет\"},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"after done\"}}]}\n\n")
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Привет" || res.Reasoning != "." || res.FinishReason != "length" || res.Usage.CompletionTokens != 2 {
		t.Errorf("result = %+v", res)
	}
	if len(deltas) != 3 || deltas[0].Reasoning != "." || deltas[2].Content != "вет" {
		t.Errorf("deltas = %+v", deltas)
	}
}

func TestChatStreamPlainJSONAnswer(t *testing.T) {
	res, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"whole"},"finish_reason":"stop"}]}`)
	}, time.Second)
	if err != nil || res.Content != "whole" || res.FinishReason != "stop" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestChatStreamErrors(t *testing.T) {
	// An error before the stream starts.
	_, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	}, time.Second)
	if !ContextExceeded(err) {
		t.Errorf("err = %v", err)
	}

	// An error in the middle of the stream keeps what arrived.
	res, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"engine died\",\"type\":\"engine_error\"}}\n\n")
	}, time.Second)
	var api *APIError
	if !errors.As(err, &api) || api.Message != "engine died" || res.Content != "half" {
		t.Errorf("res = %+v, err = %v", res, err)
	}

	// A connection that closes without a single token.
	_, _, err = stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}, time.Second)
	if err == nil {
		t.Error("empty stream: expected an error")
	}
}

func TestChatStreamIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	res, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, 150*time.Millisecond)
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("err = %v", err)
	}
	if res.Content != "x" {
		t.Errorf("partial content = %q", res.Content)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v", elapsed)
	}
}

func TestChatStreamCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := (&Client{BaseURL: srv.URL, IdleTimeout: time.Minute}).ChatStream(ctx, ChatRequest{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestServerDown(t *testing.T) {
	_, err := (&Client{BaseURL: "http://127.0.0.1:1"}).Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "запущен ли сервер") {
		t.Errorf("err = %v", err)
	}
}

// A server stopped in the middle of a reply must not pass for a finished
// one: the text so far comes back together with ErrCutShort.
func TestChatStreamCutShort(t *testing.T) {
	half := "data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n"

	// The stream simply ends: no finish reason, no [DONE].
	res, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, half)
	}, time.Second)
	if !errors.Is(err, ErrCutShort) || res.Content != "half" {
		t.Errorf("clean end: res = %+v, err = %v", res, err)
	}

	// The connection is torn down.
	res, _, err = stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, half)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}, time.Second)
	if !errors.Is(err, ErrCutShort) || res.Content != "half" {
		t.Errorf("abort: res = %+v, err = %v", res, err)
	}

	// A finish reason without the closing [DONE] is a complete answer.
	res, _, err = stream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"whole\"},\"finish_reason\":\"stop\"}]}\n\n")
	}, time.Second)
	if err != nil || res.Content != "whole" {
		t.Errorf("no [DONE]: res = %+v, err = %v", res, err)
	}
}
