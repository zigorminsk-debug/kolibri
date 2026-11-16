package pipeline

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/screenshot"
	"github.com/zigorminsk-debug/kolibri/internal/worklog"
)

func makeTestShot(t *testing.T, name string, w, h int) screenshot.Image {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{R: 30, G: 40, B: 60, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	im, err := screenshot.Decode(name, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func countImages(messages []colibri.Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Images)
	}
	return n
}

func TestRunReadsMultipleScreenshotsOnePerRequest(t *testing.T) {
	gh := newFakeGitHub(t)
	var imageCounts []int
	llm := newFakeLLM(t, func(n int, messages []colibri.Message) fakeReply {
		imgs := countImages(messages)
		if imgs > 1 {
			t.Errorf("call %d sent %d images (colibri limit is 1)", n, imgs)
		}
		imageCounts = append(imageCounts, imgs)
		switch n {
		case 0:
			return fakeReply{content: "Dialog error: panic: divide by zero at calc.go:17"}
		case 1:
			return fakeReply{content: "Console trace: main.compute(0) -> exit status 2"}
		default:
			user := messages[len(messages)-1].Content
			if !strings.Contains(user, "panic: divide by zero at calc.go:17") || !strings.Contains(user, "main.compute(0)") {
				t.Errorf("coding prompt missing screenshot transcripts:\n%s", user)
			}
			return fakeReply{content: "SUMMARY: guard zero divisor\n=== FILE: go.mod ===\nmodule demo\n\ngo 1.22\n=== END FILE ===\n=== FILE: main.go ===\npackage main\n\nfunc main() {}\n=== END FILE ==="}
		}
	})
	runner, rec := newRunner(t, llm, gh)

	task := baseTask(t)
	task.Prompt = "" // screenshot-only submission uses default crash-fix prompt
	task.Screenshots = []screenshot.Image{
		makeTestShot(t, "dialog.png", 640, 360),
		makeTestShot(t, "console.png", 800, 400),
	}
	if _, err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	if len(imageCounts) != 3 || imageCounts[0] != 1 || imageCounts[1] != 1 || imageCounts[2] != 1 {
		t.Fatalf("image counts per call = %v, want [1 1 1]", imageCounts)
	}
	logDoc := gh.headFiles("main")[worklog.Path]
	if !strings.Contains(logDoc, "**Скриншоты:** dialog.png (640×360), console.png (800×400)") {
		t.Errorf("worklog missing screenshots line:\n%s", logDoc)
	}
}

func TestRunScreenshotFallsBackToOCRWhenModelIsTextOnly(t *testing.T) {
	gh := newFakeGitHub(t)
	var sawOCR bool
	llm := newFakeLLM(t, func(n int, messages []colibri.Message) fakeReply {
		if countImages(messages) > 0 {
			return fakeReply{status: 400, errCode: "unsupported_content_type"}
		}
		if strings.Contains(messages[len(messages)-1].Content, "UnhandledPromiseRejection: EADDRINUSE :8080") {
			sawOCR = true
		}
		return fakeReply{content: "SUMMARY: handle port in use\n=== FILE: go.mod ===\nmodule demo\n\ngo 1.22\n=== END FILE ===\n=== FILE: main.go ===\npackage main\n\nfunc main() {}\n=== END FILE ==="}
	})
	runner, rec := newRunner(t, llm, gh)
	runner.OCR = func(_ context.Context, im screenshot.Image) (string, error) {
		return "Crash in " + im.Name + "\nUnhandledPromiseRejection: EADDRINUSE :8080", nil
	}

	task := baseTask(t)
	task.Screenshots = []screenshot.Image{makeTestShot(t, "crash.png", 500, 300)}
	if _, err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run with OCR fallback: %v\n%s", err, rec.journal())
	}
	if !sawOCR {
		t.Fatalf("model never received OCR text; journal:\n%s", rec.journal())
	}
}
