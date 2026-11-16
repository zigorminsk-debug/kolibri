// Package config stores the user's settings and the last task form between runs.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const appDir = "kolibri-forge"

// Config is everything the app remembers. It is written as JSON into the
// user's config directory with owner-only permissions because it holds tokens.
type Config struct {
	// Colibri (OpenAI-compatible local server).
	ColibriURL     string  `json:"colibri_url"`
	ColibriKey     string  `json:"colibri_api_key,omitempty"`
	Model          string  `json:"model,omitempty"`
	Temperature    float64 `json:"temperature"`
	MaxTokens      int     `json:"max_tokens"`       // 0 = let the server decide
	IdleTimeoutMin int     `json:"idle_timeout_min"` // abort a stream that sends nothing for this long

	// GitHub.
	GitHubToken string `json:"github_token,omitempty"`
	// GitHubClientID is the Client ID of the user's OAuth App. It switches
	// on signing in through the browser; without it the app asks for a token.
	GitHubClientID string `json:"github_client_id,omitempty"`

	// Lightpanda browser for web search and documentation lookup when Colibri
	// asks Kolibri Forge for information from the internet.
	UseWeb           bool   `json:"use_web"`
	LightpandaPath   string `json:"lightpanda_path,omitempty"`
	LightpandaURL    string `json:"lightpanda_url,omitempty"`
	LightpandaProxy  string `json:"lightpanda_proxy,omitempty"`
	LightpandaRobots bool   `json:"lightpanda_robots,omitempty"`
	SearchProvider   string `json:"search_provider,omitempty"`
	SearchAPIKey     string `json:"search_api_key,omitempty"`
	MaxWebSteps      int    `json:"max_web_steps"`
	WebTimeoutSec    int    `json:"web_timeout_sec"`

	// Where finished builds are unpacked.
	OutputDir string `json:"output_dir"`

	// Updates of the program itself. CheckUpdates asks GitHub for releases
	// at start; UpdateBuilds also offers the build-N pre-releases published
	// between versions; UpdateSkipped is the tag the user chose not to be
	// reminded of.
	CheckUpdates  bool   `json:"check_updates"`
	UpdateBuilds  bool   `json:"update_builds"`
	UpdateSkipped string `json:"update_skipped,omitempty"`

	// Last task form.
	Repo        string   `json:"repo,omitempty"` // owner/name
	Branch      string   `json:"branch,omitempty"`
	ProjectKind string   `json:"project_kind,omitempty"`
	Targets     []string `json:"targets,omitempty"`
	MaxFixes    int      `json:"max_fixes"`
	Private     bool     `json:"private"`
	Release     bool     `json:"release"`
	LastTask    string   `json:"last_task,omitempty"`
}

// Default returns the settings used on first start.
func Default() Config {
	return Config{
		ColibriURL:     "http://127.0.0.1:8000",
		Temperature:    0.2,
		IdleTimeoutMin: 15,
		UseWeb:         true,
		SearchProvider: "auto",
		MaxWebSteps:    8,
		WebTimeoutSec:  30,
		OutputDir:      defaultOutputDir(),
		ProjectKind:    "auto",
		CheckUpdates:   true,
		UpdateBuilds:   true,
		Targets:        []string{"windows", "linux"},
		MaxFixes:       3,
		Private:        true,
		Release:        true,
	}
}

func defaultOutputDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "KolibriForge"
	}
	return filepath.Join(home, "KolibriForge")
}

// Path is the location of the settings file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("не найдена папка настроек пользователя: %w", err)
	}
	return filepath.Join(dir, appDir, "config.json"), nil
}

// Load reads the settings file. A missing file is not an error: defaults are
// returned, so the first start needs no setup step.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Default(), err
	}
	return LoadFrom(path)
}

// LoadFrom reads settings from a specific file.
func LoadFrom(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("не удалось прочитать настройки: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("файл настроек %s повреждён: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// Save writes the settings file.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return c.SaveTo(path)
}

// SaveTo writes settings to a specific file, replacing it atomically.
func (c Config) SaveTo(path string) error {
	c.normalize()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("не удалось создать папку настроек: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("не удалось сохранить настройки: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("не удалось сохранить настройки: %w", err)
	}
	return nil
}

func (c *Config) normalize() {
	d := Default()
	c.ColibriURL = strings.TrimSpace(c.ColibriURL)
	if c.ColibriURL == "" {
		c.ColibriURL = d.ColibriURL
	}
	c.ColibriKey = strings.TrimSpace(c.ColibriKey)
	c.GitHubToken = strings.TrimSpace(c.GitHubToken)
	c.GitHubClientID = strings.TrimSpace(c.GitHubClientID)
	c.LightpandaPath = strings.TrimSpace(c.LightpandaPath)
	c.LightpandaURL = strings.TrimSpace(c.LightpandaURL)
	c.LightpandaProxy = strings.TrimSpace(c.LightpandaProxy)
	c.SearchProvider = strings.ToLower(strings.TrimSpace(c.SearchProvider))
	if c.SearchProvider == "" {
		c.SearchProvider = d.SearchProvider
	}
	c.SearchAPIKey = strings.TrimSpace(c.SearchAPIKey)
	c.Repo = strings.TrimSpace(c.Repo)
	c.Branch = strings.TrimSpace(c.Branch)
	if c.Temperature < 0 {
		c.Temperature = 0
	}
	if c.Temperature > 2 {
		c.Temperature = 2
	}
	if c.MaxTokens < 0 {
		c.MaxTokens = 0
	}
	if c.IdleTimeoutMin <= 0 {
		c.IdleTimeoutMin = d.IdleTimeoutMin
	}
	if c.MaxWebSteps <= 0 {
		c.MaxWebSteps = d.MaxWebSteps
	}
	if c.MaxWebSteps > 20 {
		c.MaxWebSteps = 20
	}
	if c.WebTimeoutSec < 5 {
		c.WebTimeoutSec = d.WebTimeoutSec
	}
	if c.WebTimeoutSec > 180 {
		c.WebTimeoutSec = 180
	}
	if strings.TrimSpace(c.OutputDir) == "" {
		c.OutputDir = d.OutputDir
	}
	if c.ProjectKind == "" {
		c.ProjectKind = d.ProjectKind
	}
	if len(c.Targets) == 0 {
		c.Targets = d.Targets
	}
	if c.MaxFixes < 0 {
		c.MaxFixes = 0
	}
	if c.MaxFixes > 10 {
		c.MaxFixes = 10
	}
}

// Token returns the GitHub token to use: the saved one, or the one the
// environment already provides (GITHUB_TOKEN, then GH_TOKEN).
func (c Config) Token() string {
	if c.GitHubToken != "" {
		return c.GitHubToken
	}
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// SplitRepo parses "owner/name" (a full github.com URL is accepted too).
func SplitRepo(s string) (owner, name string, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !validName(parts[0]) || !validName(parts[1]) {
		return "", "", errors.New("укажите репозиторий в виде владелец/название, например ivan/my-app")
	}
	return parts[0], parts[1], nil
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
