package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/screenshot"
)

const (
	defaultScreenshotPrompt = "Изучи приложенный скриншот ошибки или сбоя программы, найди причину и исправь проект."

	// bodyHeadroom keeps JSON framing well below colibri's 4 MiB MAX_BODY limit.
	bodyHeadroom = 220 << 10

	screenshotInspectSystem = "You are examining a user-supplied screenshot (for example a crash dialog, stack trace, terminal error, log window or broken UI of the program being developed). " +
		"Transcribe EVERY visible error message, exception type, error code, file path, function name, line number, stack trace line, button/field state and diagnostic clue verbatim, keeping the original language of quoted text. " +
		"Then briefly note the visual layout or anomaly shown. Be concise, factual and exact — your transcript will be used directly to fix the code."

	inlineScreenshotHint = "Attached screenshot: carefully inspect the image above — read every visible error message, dialog title, error code, file path, line number, stack trace, console log and UI detail, and use them to solve the task."
)

func (r *Runner) ocrFunc() func(context.Context, screenshot.Image) (string, error) {
	if r.OCR != nil {
		return r.OCR
	}
	return screenshot.ExtractText
}

func normalizeScreenshots(in []screenshot.Image) []screenshot.Image {
	out := make([]screenshot.Image, 0, min(len(in), screenshot.MaxAttached))
	for _, im := range in {
		if len(im.Data) == 0 {
			continue
		}
		if len(out) >= screenshot.MaxAttached {
			break
		}
		out = append(out, im)
	}
	return out
}

func shotLabels(shots []screenshot.Image) []string {
	if len(shots) == 0 {
		return nil
	}
	out := make([]string, len(shots))
	for i, im := range shots {
		out[i] = im.Label()
	}
	return out
}

// prepareScreenshots sets up screenshot delivery for a run.
//
// Colibri's server accepts at most one image per /v1/chat/completions call.
// When the user attaches a single screenshot it is sent straight inside the
// coding message with no extra round-trip. When two or more screenshots are
// attached, each is inspected in its own 1-image request first so every error
// dialog and stack trace is transcribed verbatim, and the first screenshot is
// also attached inline to the main coding turn. If the active colibri model is
// text-only, prepareScreenshots falls back automatically to local OCR.
func (r *Runner) prepareScreenshots(ctx context.Context, st *state) error {
	shots := st.shots
	if len(shots) == 0 {
		return nil
	}
	labels := shotLabels(shots)
	if len(shots) == 1 {
		r.logf(Info, "Приложен скриншот %s — отправляю модели %s для изучения", labels[0], st.model)
		im := shots[0]
		st.inlineShot = &im
		return nil
	}

	r.logf(Info, "Приложено %d %s (%s) — поочерёдно передаю модели %s для разбора",
		len(shots), plural(len(shots), "скриншот", "скриншота", "скриншотов"),
		strings.Join(labels, ", "), st.model)

	var notes strings.Builder
	notes.WriteString("Findings transcribed from the user's attached screenshots:\n")

	for i, shot := range shots {
		text, err := r.inspectOneScreenshot(ctx, st, shot, i+1, len(shots))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if colibri.VisionUnsupported(err) {
				return r.fallbackToOCR(ctx, st, err)
			}
			return err
		}
		fmt.Fprintf(&notes, "\n### Screenshot %d: %s\n%s\n", i+1, shot.Label(), strings.TrimSpace(text))
	}

	st.visionNotes = strings.TrimSpace(notes.String())
	first := shots[0]
	st.inlineShot = &first
	return nil
}

func (r *Runner) inspectOneScreenshot(ctx context.Context, st *state, shot screenshot.Image, idx, total int) (string, error) {
	title := fmt.Sprintf("Чтение скриншота %d из %d (%s)", idx, total, shot.Name)
	prompt := fmt.Sprintf(
		"Screenshot %d of %d (%s), attached for task:\n%s\n\nExtract all visible error text, codes, stack traces, file paths, logs and UI issues.",
		idx, total, shot.Label(), clip(strings.TrimSpace(st.task.Prompt), 800),
	)
	cur := shot
	for attempt := range 3 {
		fitted, err := fitShotToBody(cur, len(screenshotInspectSystem)+len(prompt), attempt)
		if err != nil {
			return "", err
		}
		msgs := []colibri.Message{
			{Role: "system", Content: screenshotInspectSystem},
			{
				Role:    "user",
				Content: prompt,
				Images:  []colibri.Image{{DataURI: fitted.DataURI()}},
			},
		}
		r.Obs.ModelStart(title)
		maxTok := st.task.MaxTokens
		if maxTok <= 0 || maxTok > 1400 {
			maxTok = 1400
		}
		res, err := r.LLM.ChatStream(ctx, colibri.ChatRequest{
			Model:       st.model,
			Messages:    msgs,
			Temperature: 0.1,
			MaxTokens:   maxTok,
		}, func(d colibri.Delta) {
			if d.Reasoning != "" {
				r.Obs.ModelDelta(d.Reasoning, true)
			}
			if d.Content != "" {
				r.Obs.ModelDelta(d.Content, false)
			}
		})
		if err == nil {
			out := strings.TrimSpace(res.Content)
			if out == "" {
				out = "(модель не обнаружила текста на изображении)"
			}
			return out, nil
		}
		if (colibri.ContextExceeded(err) || colibri.BodyTooLarge(err)) && attempt < 2 {
			r.logf(Warn, "Скриншот %s велик для контекста — уменьшаю разрешение и повторяю", shot.Name)
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("не удалось передать скриншот %s модели", shot.Name)
}

// fallbackToOCR extracts visible text from attached screenshots locally when
// the running colibri model does not have a vision encoder.
func (r *Runner) fallbackToOCR(ctx context.Context, st *state, visionErr error) error {
	st.visionOff = true
	st.inlineShot = nil
	r.logf(Warn, "Модель %s не принимает изображения напрямую — пробую локально распознать текст со скриншота (OCR)…", st.model)

	ocr := r.ocrFunc()
	var notes strings.Builder
	notes.WriteString("Visible text extracted via OCR from the user's attached screenshot(s) (treat minor OCR typos carefully and cross-check identifiers against the codebase):\n")
	recognized := 0
	for i, shot := range st.shots {
		txt, err := ocr(ctx, shot)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		txt = strings.TrimSpace(txt)
		if txt == "" {
			continue
		}
		recognized++
		r.logf(Info, "Распознан текст на скриншоте %s (%d симв.)", shot.Name, len([]rune(txt)))
		fmt.Fprintf(&notes, "\n### Screenshot %d: %s\n```\n%s\n```\n", i+1, shot.Label(), clip(txt, 6000))
	}
	if recognized == 0 {
		return fmt.Errorf(
			"модель «%s» на сервере Колибри запущена без поддержки изображений, а локально распознать текст со скриншота не удалось. "+
				"Запустите мультимодальную модель в colibri (например Qwen 3.6/3.8, GLM 5.3, DeepSeek 4.1, MiMo или Mistral Large 4 / LFM3 с ключом --mmproj), "+
				"либо вставьте текст ошибки в поле задачи (%v)",
			st.model, visionErr,
		)
	}
	st.visionNotes = strings.TrimSpace(notes.String())
	return nil
}

// attachInlineShot attaches at most one screenshot to the user turn of messages,
// shrinking it if needed so the serialized JSON stays below colibri.MaxBodyBytes.
func attachInlineShot(st *state, messages []colibri.Message, failure, resume string, shrink int) (screenshot.Image, bool) {
	if st.inlineShot == nil || st.visionOff || failure != "" || resume != "" || len(messages) == 0 {
		return screenshot.Image{}, false
	}
	// Find the single user message and ensure no other message carries images.
	userIdx := -1
	for i := range messages {
		messages[i].Images = nil
		if messages[i].Role == "user" {
			userIdx = i
		}
	}
	if userIdx < 0 {
		return screenshot.Image{}, false
	}
	fitted, err := fitShotToBody(*st.inlineShot, messagesLen(messages), shrink)
	if err != nil {
		return screenshot.Image{}, false
	}
	messages[userIdx].Images = []colibri.Image{{DataURI: fitted.DataURI()}}
	return fitted, true
}

func fitShotToBody(im screenshot.Image, textBytes, shrink int) (screenshot.Image, error) {
	maxBytes := colibri.MaxBodyBytes - bodyHeadroom - textBytes
	// Convert base64 budget back to raw binary image bytes (3/4).
	maxRaw := maxBytes * 3 / 4
	if maxRaw < 24<<10 {
		return screenshot.Image{}, fmt.Errorf("слишком большой текст запроса (%s) — не остаётся места для скриншота", sizeText(textBytes))
	}
	maxDim := screenshot.DefaultMaxDim
	switch {
	case shrink >= 2:
		maxDim = 720
		maxRaw = min(maxRaw, 180<<10)
	case shrink == 1:
		maxDim = 1080
		maxRaw = min(maxRaw, 360<<10)
	default:
		maxRaw = min(maxRaw, screenshot.DefaultMaxBytes)
	}
	if len(im.Data) <= maxRaw && im.Width <= maxDim && im.Height <= maxDim {
		return im, nil
	}
	return im.Shrink(maxDim, maxRaw)
}
