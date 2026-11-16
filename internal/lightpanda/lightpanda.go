// Package lightpanda drives the Lightpanda headless browser
// (github.com/lightpanda-io/browser) over its native Model Context Protocol
// interface — either as a local subprocess (native on Linux and macOS, via
// WSL2 or Docker on Windows) or over Streamable HTTP (`lightpanda mcp --port`).
package lightpanda

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Execution modes reported in Status.Mode.
const (
	ModeLocal  = "local"
	ModeHTTP   = "http"
	ModeWSL    = "wsl"
	ModeDocker = "docker"
)

// SearchProvider describes one of Lightpanda's built-in search backends.
type SearchProvider struct {
	ID     string // saved in config; "" is automatic cascade
	Title  string // shown in Settings
	EnvVar string // environment variable Lightpanda reads
}

// SearchProviders returns the supported search engines in display order.
func SearchProviders() []SearchProvider {
	return []SearchProvider{
		{ID: "", Title: "Авто (без ключа + резерв DuckDuckGo)"},
		{ID: "brave", Title: "Brave Search (BRAVE_API_KEY)", EnvVar: "BRAVE_API_KEY"},
		{ID: "tavily", Title: "Tavily (TAVILY_API_KEY)", EnvVar: "TAVILY_API_KEY"},
		{ID: "exa", Title: "Exa (EXA_API_KEY)", EnvVar: "EXA_API_KEY"},
		{ID: "parallel", Title: "Parallel (PARALLEL_API_KEY)", EnvVar: "PARALLEL_API_KEY"},
		{ID: "keenable", Title: "Keenable (KEENABLE_API_KEY)", EnvVar: "KEENABLE_API_KEY"},
	}
}

// SearchEnvVar maps a provider id to the environment variable Lightpanda reads.
func SearchEnvVar(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range SearchProviders() {
		if p.ID == id {
			return p.EnvVar
		}
	}
	return ""
}

// Status describes a working Lightpanda installation or server.
type Status struct {
	Version  string
	Mode     string // ModeLocal, ModeHTTP, ModeWSL or ModeDocker
	Location string
}

// Label is a short description for header indicators.
func (s Status) Label() string {
	v := s.Version
	if v == "" {
		v = "готов"
	}
	switch s.Mode {
	case ModeWSL:
		return v + " · WSL"
	case ModeDocker:
		return v + " · Docker"
	case ModeHTTP:
		return v + " · MCP"
	default:
		return v
	}
}

// Detail describes the found browser for the journal and Settings.
func (s Status) Detail() string {
	v := s.Version
	if v == "" {
		v = "неизвестная версия"
	}
	switch s.Mode {
	case ModeWSL:
		return fmt.Sprintf("Lightpanda %s (WSL2, %s)", v, s.Location)
	case ModeDocker:
		return fmt.Sprintf("Lightpanda %s (Docker, %s)", v, s.Location)
	case ModeHTTP:
		return fmt.Sprintf("Lightpanda %s (MCP-сервер %s)", v, s.Location)
	default:
		return fmt.Sprintf("Lightpanda %s (%s)", v, s.Location)
	}
}

// ErrNotInstalled reports that no Lightpanda binary or server was found.
var ErrNotInstalled = errors.New("браузер Lightpanda не найден")

// Driver checks Lightpanda availability and opens isolated browsing sessions.
type Driver interface {
	Check(ctx context.Context) (Status, error)
	Open(ctx context.Context) (Session, error)
}

// Session is one browsing context for a pipeline run: pages opened with `open`
// stay in memory for follow-up `links`, `extract`, `click` and `fill` calls.
type Session interface {
	Do(ctx context.Context, action Action) Result
	Close() error
}

// Browser configures how Kolibri Forge locates, installs and launches Lightpanda.
type Browser struct {
	// BinaryPath is an optional path to the lightpanda executable. On Windows
	// paths starting with "/" or "~/" refer to files inside WSL2.
	BinaryPath string
	// MCPURL, when non-empty, connects to an already-running
	// `lightpanda mcp --port N` server over HTTP instead of starting a process.
	MCPURL string
	// ManagedDir overrides <UserConfigDir>/kolibri-forge/bin (tests).
	ManagedDir string

	// SearchProvider and SearchAPIKey configure Lightpanda's built-in search.
	SearchProvider string
	SearchAPIKey   string

	// Proxy is passed to Lightpanda as --http-proxy when set.
	Proxy string
	// ObeyRobots adds --obey-robots when launching Lightpanda.
	ObeyRobots bool

	// ActionTimeout bounds one browser action (default 30s).
	ActionTimeout time.Duration
	// MaxResultBytes bounds the text handed back to the model per action (default 14 KB).
	MaxResultBytes int

	// HTTP, ReleasesURL, AllowLoopback, SearchFallbackURL, GOOS and GOARCH are
	// overrides used in tests.
	HTTP              *http.Client
	ReleasesURL       string
	AllowLoopback     bool
	SearchFallbackURL string
	GOOS              string
	GOARCH            string
}

func (b *Browser) actionTimeout() time.Duration {
	if b.ActionTimeout > 0 {
		return b.ActionTimeout
	}
	return defaultActionTimeout
}

func (b *Browser) maxResultBytes() int {
	if b.MaxResultBytes > 0 {
		return b.MaxResultBytes
	}
	return defaultMaxResultBytes
}

// Check verifies that Lightpanda is reachable and returns its version and mode.
func (b *Browser) Check(ctx context.Context) (Status, error) {
	if raw := strings.TrimSpace(b.MCPURL); raw != "" {
		conn, info, err := newHTTPConn(ctx, raw, b.HTTP)
		if err != nil {
			return Status{}, err
		}
		_ = conn.close()
		ver := strings.TrimPrefix(strings.TrimSpace(info.ServerInfo.Version), "v")
		if ver == "" {
			ver = "готов"
		}
		return Status{Version: ver, Mode: ModeHTTP, Location: raw}, nil
	}
	st, _, err := b.resolveLocal(ctx)
	return st, err
}

// Open starts an isolated Lightpanda MCP session.
func (b *Browser) Open(ctx context.Context) (Session, error) {
	var conn mcpConn
	if raw := strings.TrimSpace(b.MCPURL); raw != "" {
		c, _, err := newHTTPConn(ctx, raw, b.HTTP)
		if err != nil {
			return nil, err
		}
		conn = c
	} else {
		_, spawn, err := b.resolveLocal(ctx)
		if err != nil {
			return nil, err
		}
		c, _, err := newStdioConn(ctx, spawn)
		if err != nil {
			return nil, err
		}
		conn = c
	}
	return &mcpSession{
		conn:              conn,
		timeout:           b.actionTimeout(),
		maxBytes:          b.maxResultBytes(),
		allowLoopback:     b.AllowLoopback,
		skipDNS:           strings.TrimSpace(b.Proxy) != "",
		searchFallbackURL: b.SearchFallbackURL,
	}, nil
}

// mcpArgs returns the CLI arguments passed to `lightpanda` in stdio MCP mode.
func (b *Browser) mcpArgs() []string {
	args := []string{"mcp", "--log-level", "fatal"}
	if b.ObeyRobots {
		args = append(args, "--obey-robots")
	}
	if proxy := strings.TrimSpace(b.Proxy); proxy != "" {
		args = append(args, "--http-proxy", proxy)
	}
	if !b.AllowLoopback {
		args = append(args, "--block-private-networks", "--block-urls", blockURLPatterns)
	}
	return args
}

// isolatedEnv builds a clean environment for the Lightpanda subprocess:
// telemetry and core dumps are off, search API keys are passed when available,
// and Kolibri Forge's own secrets (GITHUB_TOKEN, GH_TOKEN, COLI_API_KEY) are
// never exposed to browser scripts or MCP tools.
func (b *Browser) isolatedEnv() []string {
	keep := []string{
		"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
		"SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC",
		"TMPDIR", "TEMP", "TMP", "XDG_CACHE_HOME", "XDG_CONFIG_HOME",
		"LANG", "LC_ALL",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		// Pass through search API keys the user set in their shell.
		"BRAVE_API_KEY", "TAVILY_API_KEY", "EXA_API_KEY", "PARALLEL_API_KEY", "KEENABLE_API_KEY",
	}
	var env []string
	overrideVar, overrideVal := b.searchKeyEnv()
	for _, k := range keep {
		if k == overrideVar && overrideVal != "" {
			continue
		}
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"LIGHTPANDA_DISABLE_TELEMETRY=true",
		"LIGHTPANDA_DISABLE_CORE_DUMP=1",
	)
	if overrideVar != "" && overrideVal != "" {
		env = append(env, overrideVar+"="+overrideVal)
	}
	return env
}

func (b *Browser) searchKeyEnv() (name, value string) {
	key := strings.TrimSpace(b.SearchAPIKey)
	if key == "" {
		return "", ""
	}
	if name := SearchEnvVar(b.SearchProvider); name != "" {
		return name, key
	}
	// If the user pasted a key while leaving the provider on "auto", pick
	// Tavily for tvly-* keys and Brave otherwise.
	if strings.HasPrefix(key, "tvly-") {
		return "TAVILY_API_KEY", key
	}
	return "BRAVE_API_KEY", key
}

// resolveLocal locates a local Lightpanda executable (native, WSL2 or Docker)
// and returns both its Status and a launcher for `lightpanda mcp`.
func (b *Browser) resolveLocal(ctx context.Context) (Status, func() (*exec.Cmd, error), error) {
	custom := strings.TrimSpace(b.BinaryPath)
	goos := b.goos()

	// 1. Explicit path from Settings.
	if custom != "" {
		if goos == "windows" && (strings.HasPrefix(custom, "/") || strings.HasPrefix(custom, "~/")) {
			expr := custom
			if strings.HasPrefix(expr, "~/") {
				expr = "$HOME/" + strings.TrimPrefix(expr, "~/")
			}
			if !safeWSLPath(expr) {
				return Status{}, nil, fmt.Errorf("недопустимые символы в пути WSL %q", custom)
			}
			ver, err := wslRunVersion(ctx, expr)
			if err != nil {
				return Status{}, nil, fmt.Errorf("не удалось запустить Lightpanda в WSL (%s): %w", custom, err)
			}
			return Status{Version: ver, Mode: ModeWSL, Location: custom}, func() (*exec.Cmd, error) {
				return b.wslMCPCommand(expr), nil
			}, nil
		}
		ver, err := b.execVersion(ctx, custom)
		if err != nil {
			return Status{}, nil, fmt.Errorf("не удалось запустить Lightpanda по пути %s: %w", custom, err)
		}
		return Status{Version: ver, Mode: ModeLocal, Location: custom}, func() (*exec.Cmd, error) {
			cmd := exec.Command(custom, b.mcpArgs()...)
			cmd.Env = b.isolatedEnv()
			return cmd, nil
		}, nil
	}

	// 2. Native binary managed by Kolibri Forge or already in PATH.
	var candidates []string
	if dir, err := b.managedDir(); err == nil {
		name := "lightpanda"
		if goos == "windows" {
			name = "lightpanda.exe"
		}
		candidates = append(candidates, filepath.Join(dir, name))
	}
	if found, err := exec.LookPath("lightpanda"); err == nil {
		candidates = append(candidates, found)
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		ver, err := b.execVersion(ctx, path)
		if err != nil {
			continue
		}
		p := path
		return Status{Version: ver, Mode: ModeLocal, Location: p}, func() (*exec.Cmd, error) {
			cmd := exec.Command(p, b.mcpArgs()...)
			cmd.Env = b.isolatedEnv()
			return cmd, nil
		}, nil
	}

	// 3. On Windows, Lightpanda runs as a Linux binary inside WSL2 or Docker.
	if goos == "windows" {
		if expr, ver, ok := wslFindBinary(ctx); ok {
			loc := strings.Replace(expr, "$HOME/", "~/", 1)
			return Status{Version: ver, Mode: ModeWSL, Location: loc}, func() (*exec.Cmd, error) {
				return b.wslMCPCommand(expr), nil
			}, nil
		}
		if dockerHasImage(ctx) {
			if ver, err := dockerVersion(ctx); err == nil {
				return Status{Version: ver, Mode: ModeDocker, Location: DockerImage}, func() (*exec.Cmd, error) {
					return b.dockerMCPCommand(), nil
				}, nil
			}
		}
		return Status{}, nil, fmt.Errorf("%w: нажмите «Установить Lightpanda» в настройках (через WSL2 или Docker) либо укажите адрес MCP-сервера Lightpanda", ErrNotInstalled)
	}

	return Status{}, nil, fmt.Errorf("%w: нажмите «Установить Lightpanda» в настройках или укажите путь к программе", ErrNotInstalled)
}

func (b *Browser) execVersion(ctx context.Context, path string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, path, "version")
	hideWindow(cmd)
	cmd.Env = b.isolatedEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", errors.New(oneLine(s, 180))
		}
		return "", err
	}
	return cleanVersion(string(out)), nil
}

// wslMCPCommand runs `lightpanda mcp` inside WSL2 without exposing any secret
// on the command line: the search key is passed through WSLENV in the process
// environment, and positional arguments are forwarded via sh's "$@".
func (b *Browser) wslMCPCommand(pathExpr string) *exec.Cmd {
	execTarget := `"` + pathExpr + `"`
	if pathExpr == "lightpanda" {
		execTarget = "lightpanda"
	}
	script := `export LIGHTPANDA_DISABLE_TELEMETRY=true LIGHTPANDA_DISABLE_CORE_DUMP=1; exec ` + execTarget + ` "$@"`
	argv := append([]string{"-e", "sh", "-c", script, "sh"}, b.mcpArgs()...)
	cmd := exec.Command("wsl.exe", argv...)
	env := b.isolatedEnv()
	if k, v := b.searchKeyEnv(); k != "" && v != "" {
		env = append(env, "WSLENV="+k+"/u")
	}
	cmd.Env = env
	return cmd
}

func (b *Browser) dockerMCPCommand() *exec.Cmd {
	argv := []string{
		"run", "--rm", "-i",
		"-e", "LIGHTPANDA_DISABLE_TELEMETRY=true",
		"-e", "LIGHTPANDA_DISABLE_CORE_DUMP=1",
	}
	if k, _ := b.searchKeyEnv(); k != "" {
		// Pass only the variable name so the key value stays in cmd.Env,
		// not in `docker ps` or process listings.
		argv = append(argv, "-e", k)
	}
	argv = append(argv, DockerImage, "/bin/lightpanda")
	argv = append(argv, b.mcpArgs()...)
	cmd := exec.Command("docker", argv...)
	cmd.Env = b.isolatedEnv()
	return cmd
}

func safeWSLPath(s string) bool {
	if s == "" || len(s) > 260 {
		return false
	}
	s = strings.TrimPrefix(s, "$HOME/")
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '/', r == '.', r == '-', r == '_', r == '+':
		default:
			return false
		}
	}
	return true
}
