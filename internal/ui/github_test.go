package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/zigorminsk-debug/kolibri/internal/config"
)

// fakeHub stands in for api.github.com and github.com in UI tests.
type fakeHub struct {
	*httptest.Server
	mu     sync.Mutex
	scopes string // X-OAuth-Scopes for the token "good"
	repos  []map[string]any
	polls  []string // answers of the device-flow token endpoint, in order
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{scopes: "repo, workflow"}
	day := 24 * time.Hour
	repo := func(name, about, lang string, private, push, archived bool, age time.Duration) map[string]any {
		return map[string]any{
			"full_name": name, "description": about, "language": lang, "private": private, "archived": archived,
			"default_branch": "main", "pushed_at": time.Now().Add(-age).UTC().Format(time.RFC3339),
			"permissions": map[string]bool{"push": push},
		}
	}
	h.repos = []map[string]any{
		repo("zigorminsk-debug/calc", "Оконный калькулятор с историей вычислений", "Go", true, true, false, 2*time.Hour),
		repo("zigorminsk-debug/kolibri", "Kolibri Forge: задача → код → GitHub Actions → программа", "Go", false, true, false, 3*day),
		repo("zigorminsk-debug/notes-sync", "", "Python", true, true, false, 12*day),
		repo("minsk-makers/weather-station", "Прошивка и панель метеостанции на ESP32, с очень длинным описанием, которое не помещается в одну строку списка", "C++", false, true, false, 40*day),
		repo("JustVugg/colibri", "Run large MoE models locally", "C", false, false, false, 5*day),
		repo("zigorminsk-debug/old-site", "Старый сайт", "HTML", false, true, true, 800*day),
	}
	h.repos[3]["default_branch"] = "develop"
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user", "/user/repos":
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			if r.URL.Path == "/user" {
				w.Header().Set("X-OAuth-Scopes", h.scopes)
				w.Write([]byte(`{"login":"zigorminsk-debug"}`))
				return
			}
			if r.URL.Query().Get("page") != "1" {
				w.Write([]byte(`[]`))
				return
			}
			json.NewEncoder(w).Encode(h.repos)
		case "/login/device/code":
			w.Write([]byte(`{"device_code":"dev-1","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/login/oauth/access_token":
			if len(h.polls) == 0 {
				t.Error("unexpected device-flow poll")
				return
			}
			w.Write([]byte(h.polls[0]))
			h.polls = h.polls[1:]
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func hubUI(t *testing.T, h *fakeHub, token string) *UI {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	cfg := config.Default()
	cfg.ColibriURL = "http://127.0.0.1:1"
	cfg.GitHubToken = token
	u, _ := newTestUI(t, cfg, 1240, 800)
	u.ghBaseURL, u.oauthBaseURL = h.URL, h.URL
	return u
}

func TestRepoPickerFillsTheForm(t *testing.T) {
	h := newFakeHub(t)
	u := hubUI(t, h, "good")
	u.branch.SetText("feature-x")

	test.Tap(u.repoPick)
	u.bg.Wait()
	if u.repoDialog == nil || u.authDialog != nil {
		t.Fatal("a valid token must open the list, not the sign-in window")
	}
	snap(t, u.win, "20-repo-picker")

	list := findList(t, u)
	if list.Length() != len(h.repos) {
		t.Fatalf("list shows %d repositories, want %d", list.Length(), len(h.repos))
	}

	// Narrow the list, then pick.
	search := findEntry(t, u, "Поиск по названию или описанию")
	search.SetText("WEATHER")
	if list.Length() != 1 {
		t.Fatalf("search left %d rows", list.Length())
	}
	snap(t, u.win, "21-repo-search")
	search.SetText("no such thing")
	snap(t, u.win, "22-repo-search-empty")
	search.SetText("метеостанции") // matches the description
	list.Select(0)

	if u.repo.Text != "minsk-makers/weather-station" {
		t.Errorf("repo = %q", u.repo.Text)
	}
	// The old branch belonged to another repository.
	if u.branch.Text != "" || u.branch.PlaceHolder != "develop" {
		t.Errorf("branch = %q, placeholder = %q", u.branch.Text, u.branch.PlaceHolder)
	}
	if u.problem.Visible() {
		t.Errorf("unexpected problem: %s", u.problem.Text)
	}
}

func TestRepoPickerWarnsAboutReadOnlyRepository(t *testing.T) {
	h := newFakeHub(t)
	u := hubUI(t, h, "good")
	u.showRepoPicker()
	u.bg.Wait()
	findList(t, u).Select(4) // JustVugg/colibri: no push permission
	if u.repo.Text != "JustVugg/colibri" || !u.problem.Visible() || !strings.Contains(u.problem.Text, "нет права записи") {
		t.Errorf("repo = %q, problem = %q", u.repo.Text, u.problem.Text)
	}
}

func TestRepoPickerAsksToSignInWhenTokenIsNotEnough(t *testing.T) {
	h := newFakeHub(t)

	t.Run("no token", func(t *testing.T) {
		u := hubUI(t, h, "")
		u.showRepoPicker()
		if u.authDialog == nil || u.repoDialog != nil {
			t.Fatal("without a token the sign-in window must open first")
		}
		snap(t, u.win, "23-auth-token")

		// A wrong token is refused and stays in the window.
		token := findEntry(t, u, "ghp_… или github_pat_…")
		token.SetText("bad")
		test.Tap(findButton(t, u, "Войти с токеном"))
		u.bg.Wait()
		if u.cfg.GitHubToken != "" || u.repoDialog != nil {
			t.Fatal("a refused token must not be saved")
		}
		snap(t, u.win, "24-auth-refused")

		// The right one is saved and the list opens by itself.
		var saved config.Config
		u.Save = func(c config.Config) error { saved = c; return nil }
		token.SetText(" good ")
		test.Tap(findButton(t, u, "Войти с токеном"))
		u.bg.Wait()
		u.bg.Wait() // the list starts loading after the sign-in finishes
		if saved.GitHubToken != "good" || u.cfg.GitHubToken != "good" {
			t.Fatalf("saved token = %q", saved.GitHubToken)
		}
		if u.repoDialog == nil || findList(t, u).Length() != len(h.repos) {
			t.Fatal("the repository list must open after signing in")
		}
	})

	t.Run("token refused by GitHub", func(t *testing.T) {
		u := hubUI(t, h, "expired")
		u.showRepoPicker()
		u.bg.Wait()
		if u.authDialog == nil {
			t.Fatal("a refused token must lead to the sign-in window")
		}
		snap(t, u.win, "25-auth-expired")
	})

	t.Run("token without the repo scope", func(t *testing.T) {
		h.mu.Lock()
		h.scopes = "public_repo"
		h.mu.Unlock()
		defer func() { h.mu.Lock(); h.scopes = "repo, workflow"; h.mu.Unlock() }()
		u := hubUI(t, h, "good")
		u.showRepoPicker()
		u.bg.Wait()
		if u.authDialog != nil || u.repoDialog == nil {
			t.Fatal("a token that lists public repositories still opens the list")
		}
		snap(t, u.win, "26-repo-public-only")
	})
}

func TestSignInThroughBrowser(t *testing.T) {
	h := newFakeHub(t)
	h.polls = []string{`{"error":"authorization_pending"}`, `{"access_token":"good","token_type":"bearer"}`}
	u := hubUI(t, h, "")
	u.cfg.GitHubClientID = "client-1"
	var saved config.Config
	u.Save = func(c config.Config) error { saved = c; return nil }

	// Hold the first poll so the window can be looked at with the code on it.
	reached, release := make(chan struct{}), make(chan struct{})
	first := true
	u.deviceSleep = func(ctx context.Context, _ time.Duration) error {
		if first {
			first = false
			close(reached)
			<-release
		}
		return ctx.Err()
	}

	done := false
	u.showAuth("Чтобы показать ваши репозитории, войдите в GitHub.", func() { done = true })
	snap(t, u.win, "27-auth-browser")
	test.Tap(findButton(t, u, "Войти через GitHub"))
	<-reached
	snap(t, u.win, "28-auth-code")
	close(release)
	u.bg.Wait()

	if saved.GitHubToken != "good" || !done {
		t.Errorf("token = %q, done = %v", saved.GitHubToken, done)
	}
}

func TestSignInThroughBrowserRefused(t *testing.T) {
	h := newFakeHub(t)
	h.polls = []string{`{"error":"access_denied"}`}
	u := hubUI(t, h, "")
	u.cfg.GitHubClientID = "client-1"
	u.deviceSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	u.showAuth("", nil)
	test.Tap(findButton(t, u, "Войти через GitHub"))
	u.bg.Wait()
	if u.cfg.GitHubToken != "" {
		t.Error("a refused sign-in must not save a token")
	}
	// The window stays, offering another try.
	findButton(t, u, "Запросить новый код")
	snap(t, u.win, "29-auth-denied")
}

func TestRunWithoutTokenOpensSignIn(t *testing.T) {
	h := newFakeHub(t)
	u := hubUI(t, h, "")
	u.repo.SetText("me/app")
	u.prompt.SetText("сделай")
	u.onRunTapped()
	if u.running || u.authDialog == nil {
		t.Errorf("running = %v, sign-in window open = %v", u.running, u.authDialog != nil)
	}
}

func TestAgo(t *testing.T) {
	day := 24 * time.Hour
	cases := map[time.Duration]string{
		10 * time.Minute: "только что",
		2 * time.Hour:    "2 часа назад",
		21 * time.Hour:   "21 час назад",
		5 * day:          "5 дней назад",
		62 * day:         "2 месяца назад",
		800 * day:        "2 года назад",
	}
	for d, want := range cases {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}
