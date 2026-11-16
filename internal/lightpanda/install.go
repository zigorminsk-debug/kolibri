package lightpanda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// ReleaseRepo is the official Lightpanda repository on GitHub.
	ReleaseRepo = "lightpanda-io/browser"
	// ProjectPage is opened by the link in Settings and Help.
	ProjectPage = "https://github.com/lightpanda-io/browser"
	// DockerImage is the official container image used when Docker drives Lightpanda.
	DockerImage = "lightpanda/browser:latest"
	// wslManagedRel is where Install places the Linux binary inside WSL2.
	wslManagedRel = ".local/share/kolibri-forge/bin/lightpanda"

	defaultGitHubAPI = "https://api.github.com"
	maxBinaryBytes   = 256 << 20
)

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// AssetName returns the official release asset name for an OS and CPU pair,
// or "" when Lightpanda publishes no native binary for it (notably Windows,
// which runs the Linux binary inside WSL2 or Docker).
func AssetName(goos, goarch string) string {
	var arch string
	switch goarch {
	case "amd64", "x86_64":
		arch = "x86_64"
	case "arm64", "aarch64":
		arch = "aarch64"
	default:
		return ""
	}
	switch goos {
	case "linux":
		return "lightpanda-" + arch + "-linux"
	case "darwin":
		return "lightpanda-" + arch + "-macos"
	default:
		return ""
	}
}

// PickReleaseAsset finds the newest stable release carrying wantName; if only
// pre-releases or nightly builds exist, it falls back to the first matching one.
func PickReleaseAsset(releases []ghRelease, wantName string) (tag string, asset ghAsset, ok bool) {
	if wantName == "" {
		return "", ghAsset{}, false
	}
	var fallbackTag string
	var fallback ghAsset
	for _, r := range releases {
		if r.Draft {
			continue
		}
		for _, a := range r.Assets {
			if a.Name != wantName || a.URL == "" {
				continue
			}
			if !r.Prerelease {
				return r.TagName, a, true
			}
			if fallback.URL == "" {
				fallbackTag, fallback = r.TagName, a
			}
		}
	}
	if fallback.URL != "" {
		return fallbackTag, fallback, true
	}
	return "", ghAsset{}, false
}

// DefaultManagedDir returns <UserConfigDir>/kolibri-forge/bin, where Install
// stores the downloaded Lightpanda executable on Linux and macOS.
func DefaultManagedDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("не найдена папка настроек пользователя: %w", err)
	}
	return filepath.Join(dir, "kolibri-forge", "bin"), nil
}

// ProgressFunc reports installation progress. total is 0 when the server did
// not send Content-Length or while Docker is pulling an image.
type ProgressFunc func(downloaded, total int64, stage string)

// Install downloads the official Lightpanda release from GitHub and checks
// that it starts. On Linux and macOS it writes the native binary into
// ManagedDir; on Windows it installs the Linux binary into WSL2 (or pulls
// the official Docker image when WSL2 is absent and Docker is running).
func (b *Browser) Install(ctx context.Context, progress ProgressFunc) (Status, error) {
	report := func(done, total int64, stage string) {
		if progress != nil {
			progress(done, total, stage)
		}
	}
	goos := b.goos()
	goarch := b.goarch()

	if goos == "windows" {
		return b.installWindows(ctx, report)
	}

	want := AssetName(goos, goarch)
	if want == "" {
		return Status{}, fmt.Errorf("для системы %s/%s нет готовой сборки Lightpanda", goos, goarch)
	}
	dir, err := b.managedDir()
	if err != nil {
		return Status{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Status{}, fmt.Errorf("не удалось создать папку %s: %w", dir, err)
	}

	report(0, 0, "Ищу свежий релиз Lightpanda на GitHub…")
	tag, asset, err := b.latestAsset(ctx, want)
	if err != nil {
		return Status{}, err
	}

	dest := filepath.Join(dir, "lightpanda")
	tmp := fmt.Sprintf("%s.tmp.%d", dest, os.Getpid())
	defer os.Remove(tmp)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return Status{}, fmt.Errorf("не удалось создать файл %s: %w", tmp, err)
	}
	if err := b.downloadTo(ctx, asset.URL, asset.Size, tag, f, report); err != nil {
		_ = f.Close()
		return Status{}, err
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		return Status{}, err
	}
	if err := f.Close(); err != nil {
		return Status{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return Status{}, fmt.Errorf("не удалось сохранить Lightpanda в %s: %w", dest, err)
	}
	if goos == "darwin" {
		// Best-effort removal of the download quarantine attribute on macOS.
		q := exec.CommandContext(ctx, "xattr", "-d", "com.apple.quarantine", dest)
		_ = q.Run()
	}

	report(asset.Size, asset.Size, "Проверяю установленный браузер…")
	ver, err := b.execVersion(ctx, dest)
	if err != nil {
		return Status{}, fmt.Errorf("Lightpanda скачан, но не запускается: %w", err)
	}
	return Status{Version: ver, Mode: ModeLocal, Location: dest}, nil
}

func (b *Browser) installWindows(ctx context.Context, report ProgressFunc) (Status, error) {
	// Prefer WSL2: one standalone binary in the user's home directory, fast
	// startup and direct stdio MCP with no daemon to manage.
	if arch, ok := wslArch(ctx); ok {
		want := AssetName("linux", arch)
		if want == "" {
			return Status{}, fmt.Errorf("неподдерживаемая архитектура WSL (%s)", arch)
		}
		report(0, 0, "Ищу свежий релиз Lightpanda для WSL2…")
		tag, asset, err := b.latestAsset(ctx, want)
		if err != nil {
			return Status{}, err
		}
		tmp, err := os.CreateTemp("", "lightpanda-wsl-*")
		if err != nil {
			return Status{}, fmt.Errorf("не удалось создать временный файл: %w", err)
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)

		if err := b.downloadTo(ctx, asset.URL, asset.Size, tag, tmp, report); err != nil {
			_ = tmp.Close()
			return Status{}, err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			_ = tmp.Close()
			return Status{}, err
		}

		report(asset.Size, asset.Size, "Устанавливаю Lightpanda в WSL2…")
		script := `set -e; d="$HOME/.local/share/kolibri-forge/bin"; mkdir -p "$d"; t="$d/lightpanda.tmp.$$"; cat > "$t"; chmod 0755 "$t"; mv -f "$t" "$d/lightpanda"`
		cmd := exec.CommandContext(ctx, "wsl.exe", "-e", "sh", "-c", script)
		hideWindow(cmd)
		cmd.Stdin = tmp
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		_ = tmp.Close()
		if err != nil {
			return Status{}, fmt.Errorf("не удалось записать Lightpanda в WSL2: %s (%w)", oneLine(stderr.String(), 180), err)
		}

		report(asset.Size, asset.Size, "Проверяю запуск Lightpanda в WSL2…")
		ver, err := wslRunVersion(ctx, "$HOME/"+wslManagedRel)
		if err != nil {
			return Status{}, fmt.Errorf("Lightpanda установлен в WSL2, но не запустился (%v). Нужен дистрибутив с glibc 2.35+, например Ubuntu 22.04 или 24.04 (wsl --install -d Ubuntu-24.04)", err)
		}
		return Status{Version: ver, Mode: ModeWSL, Location: "~/" + wslManagedRel}, nil
	}

	// Second choice on Windows: Docker Desktop.
	if dockerReady(ctx) {
		report(0, 0, "Скачиваю образ "+DockerImage+" через Docker…")
		pull := exec.CommandContext(ctx, "docker", "pull", DockerImage)
		hideWindow(pull)
		var stderr bytes.Buffer
		pull.Stderr = &stderr
		if err := pull.Run(); err != nil {
			return Status{}, fmt.Errorf("не удалось скачать образ Docker %s: %s (%w)", DockerImage, oneLine(stderr.String(), 180), err)
		}
		ver, err := dockerVersion(ctx)
		if err != nil {
			return Status{}, fmt.Errorf("образ %s скачан, но не запускается: %w", DockerImage, err)
		}
		return Status{Version: ver, Mode: ModeDocker, Location: DockerImage}, nil
	}

	if !b.DisableNative {
		report(0, 0, "Проверяю встроенный движок Lightpanda для Windows…")
		sess, err := newNativeSession(b)
		if err != nil {
			return Status{}, err
		}
		_ = sess.Close()
		// Smoke-test HTML5 DOM + reader Markdown conversion in memory.
		probe := parseHTML("<main><h1>Lightpanda</h1><p>ok</p></main>", "https://example.com/")
		if !strings.Contains(probe.renderMarkdown(probe.Root, renderOpts{stripClutter: true}), "# Lightpanda") {
			return Status{}, errors.New("самопроверка встроенного движка Lightpanda не прошла")
		}
		return b.nativeStatus(), nil
	}

	return Status{}, errors.New("у Lightpanda нет отдельного .exe для Windows: установите подсистему Linux командой «wsl --install» (и перезагрузите компьютер), либо запустите Docker Desktop, либо укажите адрес MCP-сервера Lightpanda на Linux-машине")
}

func (b *Browser) latestAsset(ctx context.Context, wantName string) (string, ghAsset, error) {
	base := strings.TrimRight(b.ReleasesURL, "/")
	if base == "" {
		base = defaultGitHubAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+ReleaseRepo+"/releases?per_page=10", nil)
	if err != nil {
		return "", ghAsset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "KolibriForge/1.0")

	resp, err := b.downloadClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ghAsset{}, ctx.Err()
		}
		return "", ghAsset{}, fmt.Errorf("не удалось запросить список релизов Lightpanda на GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ghAsset{}, fmt.Errorf("GitHub ответил ошибкой %d при поиске релиза Lightpanda", resp.StatusCode)
	}
	var releases []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return "", ghAsset{}, fmt.Errorf("список релизов Lightpanda не разобран: %w", err)
	}
	tag, asset, ok := PickReleaseAsset(releases, wantName)
	if !ok {
		return "", ghAsset{}, fmt.Errorf("в релизах %s не найден файл %s", ReleaseRepo, wantName)
	}
	return tag, asset, nil
}

func (b *Browser) downloadTo(ctx context.Context, rawURL string, knownSize int64, tag string, dst io.Writer, report ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "KolibriForge/1.0")

	resp, err := b.downloadClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("не удалось скачать Lightpanda: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("сервер загрузки Lightpanda ответил ошибкой %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total <= 0 {
		total = knownSize
	}
	label := "Скачиваю Lightpanda " + strings.TrimPrefix(tag, "v")
	report(0, total, label+"…")

	buf := make([]byte, 64<<10)
	var written int64
	lastReport := time.Now()
	reader := io.LimitReader(resp.Body, maxBinaryBytes+1)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBinaryBytes {
				return errors.New("файл Lightpanda неожиданно большой — загрузка прервана")
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("ошибка записи Lightpanda: %w", werr)
			}
			if time.Since(lastReport) >= 150*time.Millisecond {
				lastReport = time.Now()
				report(written, total, label+"…")
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("загрузка Lightpanda прервалась: %w", rerr)
		}
	}
	if written == 0 {
		return errors.New("скачан пустой файл Lightpanda")
	}
	report(written, total, label+" — загружено")
	return nil
}

// downloadClient never forwards Authorization headers across redirects.
func (b *Browser) downloadClient() *http.Client {
	base := http.DefaultTransport
	if b.HTTP != nil && b.HTTP.Transport != nil {
		base = b.HTTP.Transport
	}
	return &http.Client{
		Transport: base,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("слишком много перенаправлений")
			}
			if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

func (b *Browser) goos() string {
	if b.GOOS != "" {
		return b.GOOS
	}
	return runtime.GOOS
}

func (b *Browser) goarch() string {
	if b.GOARCH != "" {
		return b.GOARCH
	}
	return runtime.GOARCH
}

func (b *Browser) managedDir() (string, error) {
	if strings.TrimSpace(b.ManagedDir) != "" {
		return strings.TrimSpace(b.ManagedDir), nil
	}
	return DefaultManagedDir()
}

// ---- WSL2 & Docker probes (used on Windows) -----------------------------

func wslArch(ctx context.Context) (string, bool) {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return "", false
	}
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "wsl.exe", "-e", "uname", "-m")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	arch := strings.TrimSpace(string(out))
	switch arch {
	case "x86_64", "amd64":
		return "amd64", true
	case "aarch64", "arm64":
		return "arm64", true
	default:
		return "", false
	}
}

func wslFindBinary(ctx context.Context) (expr string, version string, ok bool) {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return "", "", false
	}
	candidates := []string{
		"$HOME/" + wslManagedRel,
		"$HOME/.local/bin/lightpanda",
		"/usr/local/bin/lightpanda",
		"/usr/bin/lightpanda",
		"lightpanda",
	}
	for _, cand := range candidates {
		if v, err := wslRunVersion(ctx, cand); err == nil && v != "" {
			return cand, v, true
		}
	}
	return "", "", false
}

func wslRunVersion(ctx context.Context, pathExpr string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	script := `LIGHTPANDA_DISABLE_TELEMETRY=true LIGHTPANDA_DISABLE_CORE_DUMP=1 exec "` + pathExpr + `" version`
	if pathExpr == "lightpanda" {
		script = `LIGHTPANDA_DISABLE_TELEMETRY=true LIGHTPANDA_DISABLE_CORE_DUMP=1 exec lightpanda version`
	}
	cmd := exec.CommandContext(c, "wsl.exe", "-e", "sh", "-c", script)
	hideWindow(cmd)
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

func dockerReady(ctx context.Context) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "docker", "info", "--format", "{{.ServerVersion}}")
	hideWindow(cmd)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func dockerHasImage(ctx context.Context) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "docker", "image", "inspect", "--format", "{{.Id}}", DockerImage)
	hideWindow(cmd)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func dockerVersion(ctx context.Context) (string, error) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "docker", "run", "--rm",
		"-e", "LIGHTPANDA_DISABLE_TELEMETRY=true",
		"-e", "LIGHTPANDA_DISABLE_CORE_DUMP=1",
		DockerImage, "/bin/lightpanda", "version")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return cleanVersion(string(out)), nil
}

func cleanVersion(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "v"))
		if line != "" && len(line) < 64 {
			return line
		}
	}
	return "ok"
}
