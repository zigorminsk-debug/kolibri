package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
	"github.com/zigorminsk-debug/kolibri/internal/pipeline"
)

// browser builds the Lightpanda client matching cfg and any active test hooks.
func (u *UI) browser(cfg config.Config) *lightpanda.Browser {
	timeout := time.Duration(cfg.WebTimeoutSec) * time.Second
	if timeout < 5*time.Second {
		timeout = 30 * time.Second
	}
	prov := strings.ToLower(strings.TrimSpace(cfg.SearchProvider))
	if prov == "auto" {
		prov = ""
	}
	return &lightpanda.Browser{
		BinaryPath:     strings.TrimSpace(cfg.LightpandaPath),
		MCPURL:         strings.TrimSpace(cfg.LightpandaURL),
		Proxy:          strings.TrimSpace(cfg.LightpandaProxy),
		ObeyRobots:     cfg.LightpandaRobots,
		SearchProvider: prov,
		SearchAPIKey:   strings.TrimSpace(cfg.SearchAPIKey),
		ActionTimeout:  timeout,
		ManagedDir:     u.lpManagedDir,
		ReleasesURL:    u.lpReleasesURL,
		GOOS:           u.goosOverride,
	}
}

// checkLightpandaInto verifies that Lightpanda is reachable and reports its
// version and launch mode in st.
func (u *UI) checkLightpandaInto(b *lightpanda.Browser, btn *widget.Button, st *status) {
	if btn != nil {
		btn.Disable()
	}
	st.set("Проверяю Lightpanda…", widget.LowImportance)
	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		info, err := b.Check(ctx)
		u.do(func() {
			if btn != nil {
				btn.Enable()
			}
			switch {
			case errors.Is(err, lightpanda.ErrNotInstalled):
				st.set("Lightpanda не установлен. Нажмите «Установить Lightpanda» или укажите адрес запущенного MCP-сервера.", widget.WarningImportance)
			case err != nil:
				st.set(capitalize(err.Error()), widget.DangerImportance)
			default:
				st.set(info.Detail()+".", widget.SuccessImportance)
			}
		})
	})
}

// installLightpandaInto downloads (or pulls into WSL2/Docker on Windows) the
// official Lightpanda release and writes progress into st.
func (u *UI) installLightpandaInto(b *lightpanda.Browser, installBtn, checkBtn *widget.Button, st *status, onInstalled func(lightpanda.Status)) {
	installBtn.Disable()
	if checkBtn != nil {
		checkBtn.Disable()
	}
	st.set("Подготавливаю установку Lightpanda…", widget.LowImportance)
	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var lastUpdate time.Time
		info, err := b.Install(ctx, func(done, total int64, stage string) {
			now := time.Now()
			if done > 0 && done < total && now.Sub(lastUpdate) < 150*time.Millisecond {
				return
			}
			lastUpdate = now
			msg := stage
			if total > 0 && done > 0 {
				msg = fmt.Sprintf("%s — %.1f из %.1f МБ", stage, float64(done)/(1<<20), float64(total)/(1<<20))
			} else if done > 0 {
				msg = fmt.Sprintf("%s — %.1f МБ", stage, float64(done)/(1<<20))
			}
			u.do(func() { st.set(msg, widget.LowImportance) })
		})
		u.do(func() {
			installBtn.Enable()
			if checkBtn != nil {
				checkBtn.Enable()
			}
			if err != nil {
				st.set(capitalize(err.Error()), widget.DangerImportance)
				return
			}
			st.set("Установлено: "+info.Detail()+".", widget.SuccessImportance)
			if onInstalled != nil {
				onInstalled(info)
			}
		})
	})
}

func searchProviderOptions() []string {
	ps := lightpanda.SearchProviders()
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Title
	}
	return out
}

func searchProviderTitle(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range lightpanda.SearchProviders() {
		if p.ID == id {
			return p.Title
		}
	}
	return lightpanda.SearchProviders()[0].Title
}

func searchProviderID(title string) string {
	for _, p := range lightpanda.SearchProviders() {
		if p.Title == title {
			if p.ID == "" {
				return "auto"
			}
			return p.ID
		}
	}
	return "auto"
}

// ---- web research tab ---------------------------------------------------

// webView lists every browser action Kolibri Forge ran through Lightpanda and
// shows the gathered search hits, page Markdown or extracted data.
type webView struct {
	steps    []pipeline.WebStep
	selected int
	list     *widget.List
	preview  *widget.TextGrid
	caption  *canvas.Text
	openBtn  *widget.Button
	copyBtn  *widget.Button
	curURL   string
	curText  string
	empty    *fyne.Container
	root     *fyne.Container
}

func newWebView(openURL, onCopy func(string)) *webView {
	v := &webView{preview: newGrid(), selected: -1}
	v.caption = text("", 12.5, colMuted, fyne.TextStyle{})
	v.openBtn = quietButton("Открыть страницу", theme.ComputerIcon(), func() {
		if v.curURL != "" {
			openURL(v.curURL)
		}
	})
	v.openBtn.Hide()
	v.copyBtn = quietButton("Копировать", theme.ContentCopyIcon(), func() {
		if v.curText != "" {
			onCopy(v.curText)
		}
	})

	v.list = widget.NewList(
		func() int { return len(v.steps) },
		func() fyne.CanvasObject {
			m := text("✓", 14, colEmerald, fyne.TextStyle{Monospace: true, Bold: true})
			m.Alignment = fyne.TextAlignCenter
			name := widget.NewLabel("#1 Поиск: «пример»")
			name.Truncation = fyne.TextTruncateEllipsis
			return container.NewBorder(nil, nil, container.NewCenter(fixed(18, 20, m)), nil, name)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id >= len(v.steps) {
				return
			}
			st := v.steps[id]
			row := obj.(*fyne.Container)
			name := row.Objects[0].(*widget.Label)
			m := row.Objects[1].(*fyne.Container).Objects[0].(*fyne.Container).Objects[0].(*canvas.Text)
			mark, t := webStepMark(st)
			m.Text, m.Color = mark, t.color()
			m.Refresh()
			name.SetText(fmt.Sprintf("#%d %s", st.Index, st.Action.Describe()))
		},
	)
	v.list.OnSelected = func(id widget.ListItemID) { v.show(id) }

	v.empty = emptyState(
		"Запросов в интернет пока нет",
		"Когда Колибри поручает найти сведения или открыть страницу, Kolibri Forge выполняет действие в Lightpanda и показывает результат здесь.",
	)
	head := container.NewBorder(nil, nil, nil,
		container.NewHBox(v.openBtn, v.copyBtn),
		container.New(layout.NewCustomPaddedLayout(4, 4, 6, 6), v.caption),
	)
	right := container.NewBorder(head, nil, nil, nil, v.preview)
	split := container.NewHSplit(v.list, right)
	split.Offset = 0.34
	split.Hide()
	v.root = container.NewStack(v.empty, split)
	return v
}

func webStepMark(st pipeline.WebStep) (string, tone) {
	switch {
	case st.Running:
		return "…", toneBusy
	case st.Err != "":
		return "×", toneBad
	case st.Cached:
		return "↻", toneGood
	default:
		return "✓", toneGood
	}
}

func (v *webView) set(steps []pipeline.WebStep) {
	prevCount := len(v.steps)
	v.steps = steps
	v.list.Refresh()
	if len(steps) == 0 {
		v.selected = -1
		v.curURL, v.curText = "", ""
		v.list.UnselectAll()
		v.root.Objects[1].Hide()
		v.empty.Show()
		return
	}
	v.empty.Hide()
	v.root.Objects[1].Show()

	// Follow the latest step unless the user deliberately inspected an older one.
	follow := v.selected < 0 || v.selected >= prevCount-1
	if follow {
		last := len(steps) - 1
		v.list.Select(last)
		v.show(last)
	} else if v.selected < len(steps) {
		v.show(v.selected)
	}
}

func (v *webView) show(id int) {
	if id < 0 || id >= len(v.steps) {
		return
	}
	v.selected = id
	st := v.steps[id]

	parts := []string{fmt.Sprintf("Шаг #%d (раунд %d)", st.Index, st.Round)}
	switch {
	case st.Running:
		parts = append(parts, "выполняется в Lightpanda…")
	case st.Cached:
		parts = append(parts, "из кэша этого запуска")
	case st.Err != "":
		parts = append(parts, "ошибка")
	case st.Duration > 0:
		if st.Duration < time.Second {
			parts = append(parts, fmt.Sprintf("%d мс", st.Duration.Milliseconds()))
		} else {
			parts = append(parts, fmt.Sprintf("%.1f с", st.Duration.Seconds()))
		}
	}
	if st.Title != "" {
		parts = append(parts, shorten(st.Title, 48))
	}
	v.caption.Text = strings.Join(parts, " · ")
	v.caption.Refresh()

	v.curURL = ""
	if strings.HasPrefix(st.URL, "https://") || strings.HasPrefix(st.URL, "http://") {
		v.curURL = st.URL
		v.openBtn.Show()
	} else if strings.HasPrefix(st.Action.URL, "https://") || strings.HasPrefix(st.Action.URL, "http://") {
		v.curURL = st.Action.URL
		v.openBtn.Show()
	} else {
		v.openBtn.Hide()
	}

	var body strings.Builder
	body.WriteString(st.Action.Describe())
	body.WriteString("\n")
	if v.curURL != "" && !strings.Contains(st.Action.Describe(), v.curURL) {
		body.WriteString("URL: " + v.curURL + "\n")
	}
	body.WriteString("\n")
	switch {
	case st.Running:
		body.WriteString("Lightpanda выполняет действие…\n")
	case st.Err != "":
		body.WriteString("Ошибка: " + st.Err + "\n")
	case st.Content != "":
		body.WriteString(strings.ReplaceAll(st.Content, "\t", "    "))
		if !strings.HasSuffix(st.Content, "\n") {
			body.WriteString("\n")
		}
	default:
		body.WriteString("(пустой ответ)\n")
	}
	v.curText = body.String()
	v.preview.SetText(wrapTextLines(v.curText, 110))
	if rendered(v.preview) {
		v.preview.ScrollToTop()
	}
}

// wrapTextLines breaks very long prose lines on word boundaries so page text
// in the Lightpanda tab stays readable without horizontal scrolling.
func wrapTextLines(s string, width int) string {
	if width <= 20 {
		return s
	}
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		r := []rune(line)
		for len(r) > width {
			cut := width
			for i := width; i > width/2; i-- {
				if r[i] == ' ' {
					cut = i
					break
				}
			}
			out.WriteString(string(r[:cut]))
			out.WriteByte('\n')
			r = []rune(strings.TrimLeft(string(r[cut:]), " "))
		}
		out.WriteString(string(r))
		out.WriteByte('\n')
	}
	return strings.TrimSuffix(out.String(), "\n")
}
