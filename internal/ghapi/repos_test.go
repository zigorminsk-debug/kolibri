package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestListReposFollowsPages(t *testing.T) {
	const total = 230
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("sort") != "pushed" || q.Get("per_page") != "100" || !strings.Contains(q.Get("affiliation"), "organization_member") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		var page int
		fmt.Sscan(q.Get("page"), &page)
		var out []map[string]any
		for i := (page - 1) * 100; i < page*100 && i < total; i++ {
			out = append(out, map[string]any{
				"full_name": fmt.Sprintf("me/repo-%03d", i), "private": i%2 == 0, "default_branch": "main",
				"archived": i == 1, "pushed_at": "2026-11-01T10:00:00Z", "permissions": map[string]bool{"push": i != 2},
			})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	repos, more, err := (&Client{Token: "t", BaseURL: srv.URL}).ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != total || more {
		t.Fatalf("got %d repositories, more=%v", len(repos), more)
	}
	if repos[0].FullName != "me/repo-000" || !repos[0].Private || repos[0].DefaultBranch != "main" || repos[0].PushedAt.IsZero() {
		t.Errorf("first = %+v", repos[0])
	}
	if !repos[0].Writable() || repos[1].Writable() || repos[2].Writable() {
		t.Error("Writable must be false for archived and read-only repositories")
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, `{"message":"Bad credentials"}`, true},
		{403, `{"message":"Resource not accessible by personal access token"}`, true},
		{403, `{"message":"API rate limit exceeded for user"}`, false},
		{404, `{"message":"Not Found"}`, false},
		{500, `{"message":"oops"}`, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		_, _, err := (&Client{Token: "t", BaseURL: srv.URL}).ListRepos(context.Background())
		srv.Close()
		if err == nil || IsAuthError(err) != tc.want {
			t.Errorf("%d %s: IsAuthError = %v (err %v)", tc.status, tc.body, IsAuthError(err), err)
		}
	}
	if IsAuthError(errors.New("boom")) || IsAuthError(nil) {
		t.Error("non-API errors are not auth errors")
	}
}

func TestTokenURLAsksForBothScopes(t *testing.T) {
	u, err := url.Parse(TokenURL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "github.com" || u.Query().Get("scopes") != "repo,workflow" {
		t.Errorf("TokenURL = %s", TokenURL())
	}
}

// deviceServer plays GitHub's side of the device flow: each poll answers
// with the next entry of answers.
func deviceServer(t *testing.T, start string, answers ...string) (*httptest.Server, *int) {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("client_id") != "client-1" || r.Form.Get("scope") != "repo workflow" {
				t.Errorf("start form = %v", r.Form)
			}
			if strings.Contains(start, "Not Found") {
				w.WriteHeader(http.StatusNotFound)
			}
			w.Write([]byte(start))
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "dev-1" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("poll form = %v", r.Form)
			}
			if polls >= len(answers) {
				t.Errorf("unexpected poll %d", polls+1)
				return
			}
			w.Write([]byte(answers[polls]))
			polls++
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

const deviceStart = `{"device_code":"dev-1","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`

func TestDeviceFlowSignsIn(t *testing.T) {
	srv, polls := deviceServer(t, deviceStart,
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down","interval":10}`,
		`{"error":"authorization_pending"}`,
		`{"access_token":"gho_token","token_type":"bearer","scope":"repo,workflow"}`,
	)
	var waits []time.Duration
	flow := &DeviceFlow{ClientID: " client-1 ", BaseURL: srv.URL, Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}}
	code, err := flow.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code.UserCode != "WDJB-MJHT" || code.VerificationURL != "https://github.com/login/device" || time.Until(code.Expires) < 14*time.Minute {
		t.Errorf("code = %+v", code)
	}
	token, err := flow.Wait(context.Background(), code)
	if err != nil || token != "gho_token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	if *polls != 4 {
		t.Errorf("polls = %d", *polls)
	}
	// GitHub's slow_down must lengthen every later pause.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}
}

func TestDeviceFlowFailures(t *testing.T) {
	noSleep := func(context.Context, time.Duration) error { return nil }

	t.Run("refused by the user", func(t *testing.T) {
		srv, _ := deviceServer(t, deviceStart, `{"error":"access_denied"}`)
		flow := &DeviceFlow{ClientID: "client-1", BaseURL: srv.URL, Sleep: noSleep}
		code, _ := flow.Start(context.Background())
		if _, err := flow.Wait(context.Background(), code); !errors.Is(err, ErrDeviceDenied) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("code expired", func(t *testing.T) {
		srv, _ := deviceServer(t, deviceStart, `{"error":"expired_token"}`)
		flow := &DeviceFlow{ClientID: "client-1", BaseURL: srv.URL, Sleep: noSleep}
		code, _ := flow.Start(context.Background())
		if _, err := flow.Wait(context.Background(), code); !errors.Is(err, ErrDeviceExpired) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown client id", func(t *testing.T) {
		// What github.com really answers, with status 404.
		srv, _ := deviceServer(t, `{"error":"Not Found"}`)
		flow := &DeviceFlow{ClientID: "client-1", BaseURL: srv.URL, Sleep: noSleep}
		if _, err := flow.Start(context.Background()); !errors.Is(err, ErrDeviceClient) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("device flow switched off", func(t *testing.T) {
		srv, _ := deviceServer(t, `{"error":"device_flow_disabled"}`)
		flow := &DeviceFlow{ClientID: "client-1", BaseURL: srv.URL, Sleep: noSleep}
		if _, err := flow.Start(context.Background()); !errors.Is(err, ErrDeviceDisabled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no client id", func(t *testing.T) {
		if _, err := (&DeviceFlow{}).Start(context.Background()); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		srv, polls := deviceServer(t, deviceStart)
		flow := &DeviceFlow{ClientID: "client-1", BaseURL: srv.URL}
		code, _ := flow.Start(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := flow.Wait(ctx, code); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if *polls != 0 {
			t.Errorf("polled %d times after cancel", *polls)
		}
	})
}
