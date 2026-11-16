package screenshot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FromClipboard reads a screenshot image (or copied image file path) from the
// operating system clipboard.
func FromClipboard(ctx context.Context, textHint string) (Image, error) {
	// If the user copied an image file or its path, load it straight away.
	if p := clipboardPath(textHint); p != "" {
		if IsImagePath(p) {
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return LoadFile(p)
			}
		}
	}

	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	name := "clipboard-" + time.Now().Format("150405") + ".png"
	switch runtime.GOOS {
	case "windows":
		return clipboardWindows(c, name)
	case "darwin":
		return clipboardDarwin(c, name)
	default:
		return clipboardLinux(c, name)
	}
}

func clipboardPath(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return ""
	}
	s = strings.Trim(s, `"'`)
	s = strings.TrimPrefix(s, "file://")
	if runtime.GOOS == "windows" && len(s) >= 3 && s[0] == '/' && s[2] == ':' {
		s = s[1:]
	}
	return s
}

func clipboardWindows(ctx context.Context, name string) (Image, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("kolibri-clip-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)

	// Read either a bitmap captured with Win+Shift+S / PrintScreen or the first
	// image file copied in Explorer. The destination path is passed through an
	// environment variable so nothing has to be escaped in PowerShell.
	script := `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$dst = $env:KOLIBRI_CLIP_OUT
if ([Windows.Forms.Clipboard]::ContainsImage()) {
  $img = [Windows.Forms.Clipboard]::GetImage()
  if ($null -ne $img) {
    $img.Save($dst, [Drawing.Imaging.ImageFormat]::Png)
    $img.Dispose()
    Write-Output $dst
    exit 0
  }
}
if ([Windows.Forms.Clipboard]::ContainsFileDropList()) {
  foreach ($f in [Windows.Forms.Clipboard]::GetFileDropList()) {
    if ($f -match '\.(png|jpe?g|bmp|gif)$' -and (Test-Path -LiteralPath $f -PathType Leaf)) {
      Write-Output $f
      exit 0
    }
  }
}
exit 3
`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	hideWindow(cmd)
	cmd.Env = append(os.Environ(), "KOLIBRI_CLIP_OUT="+tmp)
	out, err := cmd.Output()
	if err != nil {
		return Image{}, ErrNoClipboardImage
	}
	picked := strings.TrimSpace(string(out))
	if picked == "" {
		return Image{}, ErrNoClipboardImage
	}
	im, err := LoadFile(picked)
	if err != nil {
		return Image{}, err
	}
	if picked == tmp {
		im.Name = name
	}
	return im, nil
}

func clipboardDarwin(ctx context.Context, name string) (Image, error) {
	dir, err := os.MkdirTemp("", "kolibri-clip-*")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(dir)

	pngPath := filepath.Join(dir, "clip.png")
	script := `set outFile to POSIX file (system attribute "KOLIBRI_CLIP_OUT")
set rawData to the clipboard as «class PNGf»
set fh to open for access outFile with write permission
set eof fh to 0
write rawData to fh
close access fh`
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	cmd.Env = append(os.Environ(), "KOLIBRI_CLIP_OUT="+pngPath)
	if err := cmd.Run(); err == nil {
		if im, lerr := LoadFile(pngPath); lerr == nil {
			im.Name = name
			return im, nil
		}
	}

	// Some macOS tools place TIFF on the clipboard instead of PNG.
	tiffPath := filepath.Join(dir, "clip.tiff")
	tiffScript := `set outFile to POSIX file (system attribute "KOLIBRI_CLIP_OUT")
set rawData to the clipboard as «class TIFF»
set fh to open for access outFile with write permission
set eof fh to 0
write rawData to fh
close access fh`
	cmd2 := exec.CommandContext(ctx, "osascript", "-e", tiffScript)
	cmd2.Env = append(os.Environ(), "KOLIBRI_CLIP_OUT="+tiffPath)
	if err := cmd2.Run(); err == nil {
		if exec.CommandContext(ctx, "sips", "-s", "format", "png", tiffPath, "--out", pngPath).Run() == nil {
			if im, lerr := LoadFile(pngPath); lerr == nil {
				im.Name = name
				return im, nil
			}
		}
	}
	return Image{}, ErrNoClipboardImage
}

func clipboardLinux(ctx context.Context, name string) (Image, error) {
	type tool struct {
		bin  string
		args []string
	}
	tools := []tool{
		{"wl-paste", []string{"--no-newline", "--type", "image/png"}},
		{"wl-paste", []string{"--no-newline", "--type", "image/jpeg"}},
		{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
		{"xclip", []string{"-selection", "clipboard", "-t", "image/jpeg", "-o"}},
	}
	for _, t := range tools {
		path, err := exec.LookPath(t.bin)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, t.args...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil && out.Len() > 0 && out.Len() <= MaxInputBytes {
			if im, derr := Decode(name, out.Bytes()); derr == nil {
				return im, nil
			}
		}
	}
	return Image{}, ErrNoClipboardImage
}
