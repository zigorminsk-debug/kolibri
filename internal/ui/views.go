package ui

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/pipeline"
)

// emptyState is the centred note a view shows before it has content.
func emptyState(title, body string) *fyne.Container {
	t := text(title, 15, colText, fyne.TextStyle{Bold: true})
	t.Alignment = fyne.TextAlignCenter
	b := widget.NewLabel(body)
	b.Importance = widget.LowImportance
	b.Alignment = fyne.TextAlignCenter
	b.Wrapping = fyne.TextWrapWord
	return container.NewCenter(fixed(440, 110, container.NewVBox(t, b)))
}

// ---- journal ------------------------------------------------------------

const journalLimit = 400

var urlPattern = regexp.MustCompile(`https?://[^\s<>"«»]+[^\s<>"«».,;:!?)]`)

// journal is the timeline of a run: what happened and when, with links
// that open in the browser.
type journal struct {
	rows   *fyne.Container
	dense  *container.ThemeOverride
	scroll *container.Scroll
	filled *fyne.Container
	empty  *fyne.Container
	root   *fyne.Container
	plain  []string

	// sink receives every line as well: the log file on disk.
	sink func(level pipeline.Level, msg string, at time.Time)
	// logDir is the folder of that file, "" when there is none.
	logDir  string
	openDir func(string)
	fileBtn *widget.Button
	foot    *fyne.Container
}

func newJournal(onCopy func(string)) *journal {
	j := &journal{rows: container.NewVBox()}
	j.dense = container.NewThemeOverride(j.rows, denseTheme{})
	j.scroll = container.NewVScroll(j.dense)
	j.empty = emptyState("Здесь появится ход работы",
		"Опишите задачу слева и нажмите «Запустить». Колибри напишет код, приложение отправит его в GitHub, а Actions соберёт программу.")
	copyBtn := quietButton("Копировать журнал", theme.ContentCopyIcon(), func() { onCopy(j.text()) })
	// The journal on screen is cleared by the next run; the file is not.
	j.fileBtn = quietButton("Журнал на диске", theme.FolderOpenIcon(), func() {
		if j.openDir != nil && j.logDir != "" {
			j.openDir(j.logDir)
		}
	})
	j.fileBtn.Hide()
	j.foot = container.NewHBox(layout.NewSpacer(), j.fileBtn, copyBtn)
	j.filled = container.NewBorder(nil, j.foot, nil, nil, j.scroll)
	j.filled.Hide()
	j.root = container.NewStack(j.empty, j.filled)
	return j
}

var levelMarks = map[pipeline.Level]string{pipeline.Info: "·", pipeline.Good: "✓", pipeline.Warn: "!", pipeline.Bad: "×"}

func (j *journal) add(level pipeline.Level, msg string, at time.Time) {
	j.plain = append(j.plain, at.Format("15:04:05")+"  "+levelMarks[level]+" "+msg)
	if j.sink != nil {
		j.sink(level, msg, at)
	}
	if show := j.logDir != "" && j.openDir != nil; show != j.fileBtn.Visible() {
		if show {
			j.fileBtn.Show()
		} else {
			j.fileBtn.Hide()
		}
		j.foot.Refresh()
	}

	colorName := theme.ColorNameForeground
	switch level {
	case pipeline.Good:
		colorName = theme.ColorNameSuccess
	case pipeline.Warn:
		colorName = theme.ColorNameWarning
	case pipeline.Bad:
		colorName = theme.ColorNameError
	}
	style := widget.RichTextStyle{ColorName: colorName, Inline: true}

	// Split the message into text and clickable links.
	var segments []widget.RichTextSegment
	rest := msg
	for {
		loc := urlPattern.FindStringIndex(rest)
		if loc == nil {
			break
		}
		if loc[0] > 0 {
			segments = append(segments, &widget.TextSegment{Text: rest[:loc[0]], Style: style})
		}
		raw := rest[loc[0]:loc[1]]
		if u, err := url.Parse(raw); err == nil {
			segments = append(segments, &widget.HyperlinkSegment{Text: shortURL(u), URL: u})
		} else {
			segments = append(segments, &widget.TextSegment{Text: raw, Style: style})
		}
		rest = rest[loc[1]:]
	}
	if rest != "" || len(segments) == 0 {
		segments = append(segments, &widget.TextSegment{Text: rest, Style: style})
	}
	body := widget.NewRichText(segments...)
	body.Wrapping = fyne.TextWrapWord

	stamp := widget.NewLabelWithStyle(at.Format("15:04:05"), fyne.TextAlignLeading, fyne.TextStyle{Monospace: true})
	stamp.Importance = widget.LowImportance
	markLabel := widget.NewLabelWithStyle(levelMarks[level], fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	switch level {
	case pipeline.Good:
		markLabel.Importance = widget.SuccessImportance
	case pipeline.Warn:
		markLabel.Importance = widget.WarningImportance
	case pipeline.Bad:
		markLabel.Importance = widget.DangerImportance
	default:
		markLabel.Importance = widget.LowImportance
	}
	left := container.NewHBox(container.NewVBox(stamp), container.NewVBox(fixed(18, 26, markLabel)))
	j.rows.Add(container.NewBorder(nil, nil, left, nil, body))
	if len(j.rows.Objects) > journalLimit {
		j.rows.Remove(j.rows.Objects[0])
	}
	// New rows only pick up the dense spacing when the override is refreshed.
	j.dense.Refresh()
	if !j.filled.Visible() {
		j.empty.Hide()
		j.filled.Show()
	}
	j.scroll.ScrollToBottom()
}

// shortURL keeps links readable: github.com/me/app/actions/runs/1 instead
// of the full scheme-and-all address.
func shortURL(u *url.URL) string {
	s := u.Host + u.Path
	if utf8.RuneCountInString(s) > 64 {
		r := []rune(s)
		s = string(r[:63]) + "…"
	}
	return s
}

func (j *journal) clear() {
	j.rows.RemoveAll()
	j.plain = nil
	j.filled.Hide()
	j.empty.Show()
}

func (j *journal) text() string { return strings.Join(j.plain, "\n") }

// ---- model stream -------------------------------------------------------

const (
	streamWrapDefault = 100 // columns, until the real width of the view is known
	streamMaxRows     = 20000
	streamInterval    = 120 * time.Millisecond
)

type streamChunk struct {
	text      string
	reasoning bool
	title     bool
}

var blockMarker = regexp.MustCompile(`^\s*={2,}\s*(FILE|DELETE|WEB|BROWSER|LIGHTPANDA|SEARCH|OPEN|BROWSE|GOTO|NOTES?|NEXT|END)\b`)

// streamView shows the model's reply as it is generated. Tokens arrive far
// faster than a window should repaint, so they are buffered and drawn in
// batches.
type streamView struct {
	grid    *widget.TextGrid
	follow  *widget.Check
	counter *canvas.Text
	bar     *fyne.Container
	empty   *fyne.Container
	root    *fyne.Container

	mu      sync.Mutex
	pending []streamChunk

	answer    strings.Builder // the reply text only, for copying
	chars     int
	thinking  int
	col       int
	reasoning bool
	section   int // 0 none yet, 1 reasoning, 2 answer
	started   bool
}

// wrapColumns is how many characters fit across the view right now.
func (v *streamView) wrapColumns() int {
	if !rendered(v.grid) {
		return streamWrapDefault
	}
	cell := fyne.MeasureText("M", theme.TextSize(), fyne.TextStyle{Monospace: true}).Width
	if cell <= 0 {
		return streamWrapDefault
	}
	return max(int((v.grid.Size().Width-theme.ScrollBarSize()-4)/cell), 40)
}

func newStreamView(onCopy func(string)) *streamView {
	v := &streamView{grid: newGrid()}
	v.follow = widget.NewCheck("Следить за выводом", nil)
	v.follow.SetChecked(true)
	v.counter = text("", 12.5, colMuted, fyne.TextStyle{Monospace: true})
	copyBtn := quietButton("Копировать ответ", theme.ContentCopyIcon(), func() { onCopy(v.answer.String()) })
	v.bar = container.NewHBox(v.follow, layout.NewSpacer(), container.NewCenter(v.counter), gap(8, 0), copyBtn)
	v.empty = emptyState("Модель пока молчит", "Ответ Колибри будет появляться здесь по мере генерации — вместе с размышлениями, если модель их показывает.")
	body := container.NewBorder(nil, v.bar, nil, nil, v.grid)
	body.Hide()
	v.root = container.NewStack(v.empty, body)
	return v
}

// push queues text; safe to call from any goroutine.
func (v *streamView) push(c streamChunk) {
	v.mu.Lock()
	v.pending = append(v.pending, c)
	v.mu.Unlock()
}

func (v *streamView) reset() {
	v.mu.Lock()
	v.pending = nil
	v.mu.Unlock()
	v.grid.Rows = nil
	v.grid.Refresh()
	v.answer.Reset()
	v.chars, v.thinking, v.col, v.reasoning, v.section, v.started = 0, 0, 0, false, 0, false
	v.counter.Text = ""
	v.bar.Refresh()
	v.root.Objects[1].Hide()
	v.empty.Show()
}

var (
	styleReasoning = &widget.CustomTextGridStyle{FGColor: colFaint}
	styleSection   = &widget.CustomTextGridStyle{FGColor: colMuted, TextStyle: fyne.TextStyle{Monospace: true, Bold: true}}
	styleMarker    = &widget.CustomTextGridStyle{FGColor: colEmerald, TextStyle: fyne.TextStyle{Monospace: true, Bold: true}}
	styleTitle     = &widget.CustomTextGridStyle{FGColor: colAmber, TextStyle: fyne.TextStyle{Monospace: true, Bold: true}}
)

// flush draws what has been queued. Must run on the UI goroutine.
func (v *streamView) flush() {
	v.mu.Lock()
	batch := v.pending
	v.pending = nil
	v.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if !v.started {
		v.started = true
		v.empty.Hide()
		v.root.Objects[1].Show()
	}
	wrap := v.wrapColumns()
	rows := v.grid.Rows
	newRow := func() {
		rows = append(rows, widget.TextGridRow{})
		v.col = 0
	}
	if len(rows) == 0 {
		newRow()
	}
	for _, c := range batch {
		if c.title {
			if v.col > 0 || len(rows) > 1 {
				if v.col > 0 {
					newRow()
				}
				newRow()
			}
			rows[len(rows)-1] = styledRow("▶ "+c.text, styleTitle)
			newRow()
			v.section = 0
			continue
		}
		if c.text == "" {
			continue
		}
		// Label where the model's thinking ends and its answer begins.
		section := 2
		if c.reasoning {
			section = 1
		}
		if section != v.section {
			// Colibri's keepalive is a lone dot on the reasoning channel:
			// not worth a heading of its own.
			if c.reasoning && strings.Trim(c.text, ". \n") == "" {
				continue
			}
			v.section = section
			v.reasoning = c.reasoning
			if v.col > 0 {
				newRow()
			}
			if c.reasoning {
				rows[len(rows)-1] = styledRow("размышления", styleSection)
			} else {
				rows[len(rows)-1] = styledRow("ответ", styleSection)
			}
			newRow()
		}
		var style widget.TextGridStyle
		if c.reasoning {
			style = styleReasoning
			v.thinking += utf8.RuneCountInString(c.text)
		} else {
			v.answer.WriteString(c.text)
			v.chars += utf8.RuneCountInString(c.text)
		}
		for _, r := range c.text {
			switch {
			case r == '\n':
				v.markRow(rows, len(rows)-1)
				newRow()
				continue
			case r == '\r':
				continue
			case r == '\t':
				for n := 4 - v.col%4; n > 0; n-- {
					rows[len(rows)-1].Cells = append(rows[len(rows)-1].Cells, widget.TextGridCell{Rune: ' ', Style: style})
					v.col++
				}
				continue
			}
			if v.col >= wrap {
				// Break at the last space when there is one nearby, so
				// prose is not cut in the middle of a word.
				cells := rows[len(rows)-1].Cells
				cut := -1
				for k := len(cells) - 1; k >= 0 && k >= len(cells)-24; k-- {
					if cells[k].Rune == ' ' {
						cut = k
						break
					}
				}
				var carry []widget.TextGridCell
				if cut >= 0 && r != ' ' {
					carry = append(carry, cells[cut+1:]...)
					rows[len(rows)-1].Cells = cells[:cut]
				}
				newRow()
				rows[len(rows)-1].Cells = carry
				v.col = len(carry)
				if r == ' ' {
					continue
				}
			}
			last := len(rows) - 1
			rows[last].Cells = append(rows[last].Cells, widget.TextGridCell{Rune: r, Style: style})
			v.col++
		}
	}
	if len(rows) > streamMaxRows {
		rows = rows[len(rows)-streamMaxRows:]
	}
	v.grid.Rows = rows
	v.grid.Refresh()
	if v.follow.Checked && rendered(v.grid) {
		v.grid.ScrollToBottom()
	}
	if v.thinking > 0 {
		v.counter.Text = fmt.Sprintf("размышления %s · ответ %s симв.", groupDigits(v.thinking), groupDigits(v.chars))
	} else {
		v.counter.Text = fmt.Sprintf("ответ %s симв.", groupDigits(v.chars))
	}
	v.bar.Refresh()
}

// markRow highlights the block markers that separate files in a reply, so
// the structure is visible while the text scrolls by.
func (v *streamView) markRow(rows []widget.TextGridRow, i int) {
	if v.reasoning || len(rows[i].Cells) < 6 || len(rows[i].Cells) > 200 {
		return
	}
	var b strings.Builder
	for _, c := range rows[i].Cells {
		b.WriteRune(c.Rune)
	}
	if blockMarker.MatchString(b.String()) {
		for k := range rows[i].Cells {
			rows[i].Cells[k].Style = styleMarker
		}
	}
}

func styledRow(s string, style widget.TextGridStyle) widget.TextGridRow {
	row := widget.TextGridRow{}
	for _, r := range s {
		row.Cells = append(row.Cells, widget.TextGridCell{Rune: r, Style: style})
	}
	return row
}

// newGrid makes a TextGrid that scrolls by itself. NewTextGrid alone gives
// a grid without scrolling that draws every row, visible or not.
func newGrid() *widget.TextGrid {
	g := widget.NewTextGrid()
	g.Scroll = fyne.ScrollBoth
	return g
}

// rendered reports whether a TextGrid has been laid out. Its scroll methods
// dereference state that only exists once the grid has a renderer, which a
// grid on a never-opened tab does not have.
func rendered(g *widget.TextGrid) bool {
	size := g.Size()
	return size.Width > 0 && size.Height > 0
}

// groupDigits formats 12480 as "12 480".
func groupDigits(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune('\u00a0')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- files --------------------------------------------------------------

// filesView lists what the current attempt changes and previews a file.
type filesView struct {
	files   []pipeline.FileChange
	list    *widget.List
	preview *widget.TextGrid
	caption *canvas.Text
	empty   *fyne.Container
	root    *fyne.Container
}

var statusMarks = map[string]struct {
	mark string
	tone tone
	word string
}{
	"new":     {"+", toneGood, "новый"},
	"changed": {"~", toneBusy, "изменён"},
	"deleted": {"−", toneBad, "удалён"},
}

func newFilesView() *filesView {
	v := &filesView{preview: newGrid()}
	v.preview.ShowLineNumbers = true
	v.caption = text("", 12.5, colMuted, fyne.TextStyle{})
	v.list = widget.NewList(
		func() int { return len(v.files) },
		func() fyne.CanvasObject {
			m := text("+", 14, colEmerald, fyne.TextStyle{Monospace: true, Bold: true})
			m.Alignment = fyne.TextAlignCenter
			name := widget.NewLabel("path/to/file.go")
			name.Truncation = fyne.TextTruncateEllipsis
			return container.NewBorder(nil, nil, container.NewCenter(fixed(16, 20, m)), nil, name)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id >= len(v.files) {
				return
			}
			f := v.files[id]
			row := obj.(*fyne.Container)
			name := row.Objects[0].(*widget.Label)
			m := row.Objects[1].(*fyne.Container).Objects[0].(*fyne.Container).Objects[0].(*canvas.Text)
			st := statusMarks[f.Status]
			m.Text, m.Color = st.mark, st.tone.color()
			m.Refresh()
			name.SetText(f.Path)
		},
	)
	v.list.OnSelected = func(id widget.ListItemID) { v.show(id) }
	v.empty = emptyState("Файлов пока нет", "Когда модель закончит ответ, здесь появятся файлы, которые уйдут в коммит. Нажмите на файл, чтобы посмотреть его.")
	right := container.NewBorder(container.New(layout.NewCustomPaddedLayout(2, 6, 6, 6), v.caption), nil, nil, nil, v.preview)
	split := container.NewHSplit(v.list, right)
	split.Offset = 0.3
	split.Hide()
	v.root = container.NewStack(v.empty, split)
	return v
}

func (v *filesView) set(files []pipeline.FileChange) {
	v.files = files
	v.list.UnselectAll()
	v.list.Refresh()
	if len(files) == 0 {
		v.root.Objects[1].Hide()
		v.empty.Show()
		return
	}
	v.empty.Hide()
	v.root.Objects[1].Show()
	// Open the first real source file rather than the workflow.
	first := 0
	for i, f := range files {
		if !strings.HasPrefix(f.Path, ".github/") && f.Status != "deleted" {
			first = i
			break
		}
	}
	v.list.Select(first)
}

func (v *filesView) show(id int) {
	if id < 0 || id >= len(v.files) {
		return
	}
	f := v.files[id]
	st := statusMarks[f.Status]
	if f.Status == "deleted" {
		v.caption.Text = f.Path + " — " + st.word
		v.preview.SetText("")
	} else {
		lines := strings.Count(f.Content, "\n")
		v.caption.Text = fmt.Sprintf("%s — %s, %d %s", f.Path, st.word, lines, pluralRu(lines, "строка", "строки", "строк"))
		v.preview.SetText(strings.ReplaceAll(f.Content, "\t", "    "))
	}
	v.caption.Refresh()
	if rendered(v.preview) {
		v.preview.ScrollToTop()
	}
}

// ---- build --------------------------------------------------------------

// buildView mirrors the GitHub Actions run: every job with its state.
type buildView struct {
	box     *fyne.Container
	dense   *container.ThemeOverride
	empty   *fyne.Container
	root    *fyne.Container
	openURL func(string)
}

func newBuildView(openURL func(string)) *buildView {
	v := &buildView{box: container.NewVBox(), openURL: openURL}
	v.empty = emptyState("Сборка ещё не начиналась", "После коммита GitHub Actions запустит сборку. Здесь будет видно каждое задание и шаг, на котором оно сейчас.")
	v.dense = container.NewThemeOverride(v.box, denseTheme{})
	scroll := container.NewVScroll(v.dense)
	scroll.Hide()
	v.root = container.NewStack(v.empty, scroll)
	return v
}

func (v *buildView) clear() {
	v.box.RemoveAll()
	v.root.Objects[1].Hide()
	v.empty.Show()
}

func (v *buildView) set(runs []pipeline.RunStatus) {
	v.box.RemoveAll()
	if len(runs) == 0 {
		return
	}
	v.empty.Hide()
	v.root.Objects[1].Show()
	for _, r := range runs {
		run := r.Run
		word, t := runWords(run)
		title := text(fmt.Sprintf("Сборка №%d · %s", run.RunNumber, run.Name), 15, colText, fyne.TextStyle{Bold: true})
		state := text(word, 13, t.color(), fyne.TextStyle{Bold: true})
		open := quietButton("Открыть на GitHub", theme.ComputerIcon(), func() { v.openURL(run.HTMLURL) })
		v.box.Add(container.NewBorder(nil, nil, container.NewHBox(gap(4, 0), container.NewCenter(title), gap(10, 0), container.NewCenter(state)), open))
		for _, job := range r.Jobs {
			v.box.Add(jobRow(job))
		}
		v.box.Add(gap(0, 10))
	}
	v.dense.Refresh()
}

func runWords(run ghapi.Run) (string, tone) {
	switch {
	case !run.Done() && run.Status == "in_progress":
		return "идёт", toneBusy
	case !run.Done():
		return "в очереди", toneBusy
	case run.OK():
		return "успешно", toneGood
	case run.Conclusion == "cancelled":
		return "отменена", toneBad
	}
	return "ошибка", toneBad
}

func jobRow(job ghapi.Job) fyne.CanvasObject {
	markText, t, detail := "·", toneNeutral, "ждёт очереди"
	switch {
	case job.Status == "in_progress":
		markText, t, detail = "▸", toneBusy, "выполняется"
		if step := job.CurrentStep(); step != "" {
			detail = "шаг: " + step
		}
	case job.Status != "completed":
	case job.Conclusion == "success":
		markText, t, detail = "✓", toneGood, "успешно"
	case job.Conclusion == "skipped":
		markText, t, detail = "·", toneNeutral, "пропущено"
	case job.Conclusion == "cancelled":
		markText, t, detail = "×", toneBad, "отменено"
	default:
		markText, t, detail = "×", toneBad, "ошибка"
		if step := job.FailedStep(); step != "" {
			detail = "ошибка на шаге «" + step + "»"
		}
	}
	m := text(markText, 14, t.color(), fyne.TextStyle{Bold: true})
	m.Alignment = fyne.TextAlignCenter
	name := widget.NewLabel(job.Name)
	info := widget.NewLabel(detail)
	info.Truncation = fyne.TextTruncateEllipsis
	if t == toneBad {
		info.Importance = widget.DangerImportance
	} else {
		info.Importance = widget.LowImportance
	}
	took := ""
	if !job.StartedAt.IsZero() {
		end := job.CompletedAt
		if end.IsZero() || end.Before(job.StartedAt) {
			end = time.Now()
		}
		took = clock(end.Sub(job.StartedAt))
	}
	dur := widget.NewLabelWithStyle(took, fyne.TextAlignTrailing, fyne.TextStyle{Monospace: true})
	dur.Importance = widget.LowImportance
	left := container.NewHBox(gap(6, 0), container.NewCenter(fixed(16, 20, m)), name)
	return container.NewBorder(nil, nil, left, dur, info)
}

// clock formats a duration as 04:12 or 1:04:12.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func pluralRu(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	switch {
	case n%100 >= 11 && n%100 <= 14:
		return many
	case n%10 == 1:
		return one
	case n%10 >= 2 && n%10 <= 4:
		return few
	}
	return many
}
