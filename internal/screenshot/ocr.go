package screenshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ExtractText runs local system OCR on im. It is used automatically when the
// user attaches a screenshot while running a text-only colibri model without a
// vision tower: on Windows 10/11 it uses the built-in Windows.Media.Ocr engine
// (no extra install required), on macOS the built-in Vision framework, and on
// any platform Tesseract when present in PATH.
func ExtractText(ctx context.Context, im Image) (string, error) {
	if len(im.Data) == 0 {
		return "", errors.New("пустой скриншот")
	}
	ext := ".png"
	if im.MIME == "image/jpeg" {
		ext = ".jpg"
	}
	dir, err := os.MkdirTemp("", "kolibri-ocr-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	imgPath := filepath.Join(dir, "shot"+ext)
	if err := os.WriteFile(imgPath, im.Data, 0o600); err != nil {
		return "", err
	}

	c, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "windows":
		if txt, err := ocrWindows(c, imgPath); err == nil {
			return txt, nil
		}
		return ocrTesseract(c, imgPath)
	case "darwin":
		if txt, err := ocrTesseract(c, imgPath); err == nil {
			return txt, nil
		}
		return ocrDarwinVision(c, imgPath)
	default:
		return ocrTesseract(c, imgPath)
	}
}

func ocrWindows(ctx context.Context, imgPath string) (string, error) {
	// Windows.Media.Ocr ships with Windows 10 and 11 and recognises text in the
	// user's installed keyboard/UI languages (e.g. Russian + English).
	script := `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime]
$null = [Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics.Imaging, ContentType = WindowsRuntime]
$null = [Windows.Graphics.Imaging.SoftwareBitmap, Windows.Graphics.Imaging, ContentType = WindowsRuntime]
$null = [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime]
$null = [Windows.Globalization.Language, Windows.Globalization, ContentType = WindowsRuntime]

$asTask = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
  $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation` + "`1" + `'
})[0]
function Await($op, [Type]$t) {
  $task = $asTask.MakeGenericMethod($t).Invoke($null, @($op))
  $task.Wait(-1) | Out-Null
  return $task.Result
}

$file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($env:KOLIBRI_OCR_IN)) ([Windows.Storage.StorageFile])
$stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
$decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
$bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])

$lines = New-Object System.Collections.Generic.List[string]
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
if ($null -eq $engine) {
  foreach ($tag in @('ru-RU', 'ru', 'en-US', 'en')) {
    $lang = New-Object Windows.Globalization.Language($tag)
    if ([Windows.Media.Ocr.OcrEngine]::IsLanguageSupported($lang)) {
      $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($lang)
      if ($null -ne $engine) { break }
    }
  }
}
if ($null -eq $engine) { exit 4 }
$res = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
foreach ($l in $res.Lines) { $lines.Add($l.Text) }
$stream.Dispose()
[Console]::Write(($lines -join "` + "`n" + `"))
`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	hideWindow(cmd)
	cmd.Env = append(os.Environ(), "KOLIBRI_OCR_IN="+imgPath)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoOCR, err)
	}
	return cleanOCRText(string(out)), nil
}

func ocrDarwinVision(ctx context.Context, imgPath string) (string, error) {
	if _, err := exec.LookPath("swift"); err != nil {
		return "", ErrNoOCR
	}
	code := `import Foundation
import Vision
import AppKit
let path = ProcessInfo.processInfo.environment["KOLIBRI_OCR_IN"] ?? ""
guard let img = NSImage(contentsOfFile: path),
      let cg = img.cgImage(forProposedRect: nil, context: nil, hints: nil) else { exit(2) }
let req = VNRecognizeTextRequest()
req.recognitionLevel = .accurate
req.usesLanguageCorrection = true
req.recognitionLanguages = ["ru-RU", "en-US"]
let handler = VNImageRequestHandler(cgImage: cg, options: [:])
try handler.perform([req])
for obs in req.results ?? [] {
  if let top = obs.topCandidates(1).first { print(top.string) }
}`
	cmd := exec.CommandContext(ctx, "swift", "-e", code)
	cmd.Env = append(os.Environ(), "KOLIBRI_OCR_IN="+imgPath)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoOCR, err)
	}
	return cleanOCRText(string(out)), nil
}

func ocrTesseract(ctx context.Context, imgPath string) (string, error) {
	bin, err := exec.LookPath("tesseract")
	if err != nil {
		return "", ErrNoOCR
	}
	// Prefer Russian + English together so mixed error dialogs and stack traces
	// come out intact; fall back to English or Tesseract's default pack.
	for _, args := range [][]string{
		{imgPath, "stdout", "-l", "rus+eng", "--psm", "6"},
		{imgPath, "stdout", "-l", "eng", "--psm", "6"},
		{imgPath, "stdout"},
	} {
		cmd := exec.CommandContext(ctx, bin, args...)
		hideWindow(cmd)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return cleanOCRText(out.String()), nil
		}
	}
	return "", ErrNoOCR
}

func cleanOCRText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
