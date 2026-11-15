package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("missing file must give defaults, got %v", err)
	}
	if cfg.ColibriURL != "http://127.0.0.1:8000" || cfg.MaxFixes != 3 || len(cfg.Targets) != 2 {
		t.Errorf("defaults = %+v", cfg)
	}

	cfg.GitHubToken = "  ghp_secret \n"
	cfg.Repo = "me/app"
	cfg.MaxFixes = 99
	cfg.Temperature = -1
	cfg.OutputDir = " "
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config holds a token but its mode is %v", info.Mode().Perm())
		}
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitHubToken != "ghp_secret" || got.Repo != "me/app" || got.MaxFixes != 10 || got.Temperature != 0 || got.OutputDir == " " {
		t.Errorf("loaded = %+v", got)
	}
}

func TestLoadBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if cfg.ColibriURL == "" {
		t.Error("a broken file must still yield usable defaults")
	}
}

func TestTokenFallsBackToEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "from-env")
	if got := (Config{}).Token(); got != "from-env" {
		t.Errorf("token = %q", got)
	}
	if got := (Config{GitHubToken: "saved"}).Token(); got != "saved" {
		t.Errorf("token = %q", got)
	}
}

func TestSplitRepo(t *testing.T) {
	good := map[string][2]string{
		"me/app":                           {"me", "app"},
		" Me-1/my.app_2 ":                  {"Me-1", "my.app_2"},
		"https://github.com/me/app":        {"me", "app"},
		"https://github.com/me/app.git":    {"me", "app"},
		"github.com/zigorminsk-debug/kol/": {"zigorminsk-debug", "kol"},
	}
	for in, want := range good {
		owner, name, err := SplitRepo(in)
		if err != nil || owner != want[0] || name != want[1] {
			t.Errorf("SplitRepo(%q) = %q, %q, %v", in, owner, name, err)
		}
	}
	for _, bad := range []string{"", "app", "a/b/c", "me/", "/app", "me/my app", "me/..", "я/приложение"} {
		if _, _, err := SplitRepo(bad); err == nil {
			t.Errorf("SplitRepo(%q): expected an error", bad)
		}
	}
}
