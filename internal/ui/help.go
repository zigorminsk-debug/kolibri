package ui

import (
	_ "embed"
	"net/url"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/update"
)

// The developer of the program, shown in the help and named in the installer.
const (
	DeveloperName  = "Zakharevich Igor"
	DeveloperEmail = "ziv@csl.by"
	DeveloperPhone = "+375293371412"
)

// ProjectURL is where the program is published.
const ProjectURL = "https://github.com/" + update.Owner + "/" + update.Repo

//go:embed assets/help.md
var helpText string

// aboutTitle is the last section of the help; it is built by the program,
// not written in the help file.
const aboutTitle = "О программе"

// helpSection is one page of the help.
type helpSection struct {
	Title string
	Body  string // Markdown; empty for the "about" page
}

// helpSections splits the help file into pages at the first-level headings.
func helpSections(doc string) []helpSection {
	var out []helpSection
	var body []string
	flush := func() {
		if len(out) > 0 {
			out[len(out)-1].Body = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = body[:0]
	}
	for _, line := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		if title, ok := strings.CutPrefix(line, "# "); ok {
			flush()
			out = append(out, helpSection{Title: strings.TrimSpace(title)})
			continue
		}
		body = append(body, line)
	}
	flush()
	return append(out, helpSection{Title: aboutTitle})
}

// helpView is the content of the help window.
type helpView struct {
	sections []helpSection
	list     *widget.List
	page     *fyne.Container
	scroll   *container.Scroll
	current  int
	// about page: the result of "check for updates"
	updateStatus *status
	checkUpdate  *widget.Button
}

// showHelp opens the help window on a section (by title; the first one when
// empty or unknown). A second call brings the open window forward.
func (u *UI) showHelp(section string) {
	if u.helpWin != nil {
		u.helpView.open(u.helpView.index(section))
		u.helpWin.RequestFocus()
		return
	}
	v := &helpView{sections: helpSections(helpText), current: -1}
	v.page = container.NewStack()
	v.scroll = container.NewVScroll(container.New(layout.NewCustomPaddedLayout(6, 16, 18, 22), v.page))
	v.list = widget.NewList(
		func() int { return len(v.sections) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.ListItemID, o fyne.CanvasObject) { o.(*widget.Label).SetText(v.sections[id].Title) },
	)
	v.list.OnSelected = func(id widget.ListItemID) { u.helpPage(v, id) }

	side := container.NewBorder(
		container.New(layout.NewCustomPaddedLayout(10, 6, 12, 8), text("Справка", 17, colText, fyne.TextStyle{Bold: true})),
		nil, nil, nil, v.list)
	split := container.NewHSplit(panel(side), panel(v.scroll))
	split.Offset = 0.3

	w := u.app.NewWindow("Справка — " + AppName)
	w.SetContent(container.NewStack(canvas.NewRectangle(colInk), container.New(layout.NewCustomPaddedLayout(10, 12, 14, 14), split)))
	w.Resize(fyne.NewSize(980, 700))
	w.SetOnClosed(func() { u.helpWin, u.helpView = nil, nil })
	u.helpWin, u.helpView = w, v
	v.open(v.index(section))
	w.Show()
}

func (v *helpView) index(title string) int {
	for i, s := range v.sections {
		if s.Title == title {
			return i
		}
	}
	return 0
}

// open selects a section; the list then shows the page.
func (v *helpView) open(i int) {
	if i == v.current {
		return
	}
	v.list.Select(i)
}

func (u *UI) helpPage(v *helpView, i int) {
	if i < 0 || i >= len(v.sections) || i == v.current {
		return
	}
	v.current = i
	s := v.sections[i]
	var content fyne.CanvasObject
	if s.Title == aboutTitle {
		content = u.aboutPage(v)
	} else {
		body := widget.NewRichTextFromMarkdown("# " + s.Title + "\n\n" + s.Body)
		body.Wrapping = fyne.TextWrapWord
		content = body
	}
	v.page.Objects = []fyne.CanvasObject{content}
	v.page.Refresh()
	v.scroll.ScrollToTop()
}

// aboutPage names the program, its version and its developer, and checks
// for updates on request.
func (u *UI) aboutPage(v *helpView) fyne.CanvasObject {
	logo := canvas.NewImageFromResource(mark)
	logo.FillMode = canvas.ImageFillContain
	title := container.NewHBox(fixed(52, 52, logo), gap(8, 0),
		container.NewVBox(
			text(AppName, 22, colText, fyne.TextStyle{Bold: true}),
			text(u.versionLine(), 13, colMuted, fyne.TextStyle{}),
		))

	about := widget.NewLabel("Связывает локальную модель Колибри с GitHub: вы описываете задачу, модель пишет код, GitHub Actions собирает программу — для Windows, Linux, macOS и Android.")
	about.Wrapping = fyne.TextWrapWord

	// Contact lines: what it is, the value, and a button that copies it.
	mid := func(o fyne.CanvasObject) fyne.CanvasObject {
		return container.NewVBox(layout.NewSpacer(), o, layout.NewSpacer())
	}
	caption := func(s string) fyne.CanvasObject {
		l := widget.NewLabel(s)
		l.Importance = widget.LowImportance
		return mid(l)
	}
	copyable := func(shown fyne.CanvasObject, value string) fyne.CanvasObject {
		return container.NewHBox(mid(shown), mid(iconButton(theme.ContentCopyIcon(), func() { u.copyText(value) })))
	}
	name := widget.NewLabel(DeveloperName)
	name.TextStyle = fyne.TextStyle{Bold: true}
	developer := container.New(layout.NewFormLayout(),
		caption("Имя"), mid(name),
		caption("E-mail"), copyable(widget.NewHyperlink(DeveloperEmail, &url.URL{Scheme: "mailto", Opaque: DeveloperEmail}), DeveloperEmail),
		caption("Телефон"), copyable(widget.NewLabel(DeveloperPhone), DeveloperPhone),
		caption("Проект"), mid(widget.NewHyperlink(strings.TrimPrefix(ProjectURL, "https://"), mustURL(ProjectURL))),
	)

	v.updateStatus = newStatus()
	v.updateStatus.after = func() { v.page.Refresh() }
	v.checkUpdate = widget.NewButtonWithIcon("Проверить обновления", theme.ViewRefreshIcon(), func() {
		u.checkUpdatesInto(v.checkUpdate, v.updateStatus)
	})

	credits := hint("Написана на Go с библиотекой Fyne. Шрифты Onest и JetBrains Mono распространяются по лицензии SIL Open Font License. Модель запускает сервер colibrì — это отдельный проект.")

	return container.NewVBox(
		gap(0, 6),
		title,
		gap(0, 4),
		about,
		sectionTitle("Разработчик"),
		developer,
		sectionTitle("Обновления"),
		container.NewHBox(v.checkUpdate),
		v.updateStatus.label,
		sectionTitle("Благодарности"),
		credits,
	)
}

// checkUpdatesInto runs a manual check and writes its result under button.
func (u *UI) checkUpdatesInto(button *widget.Button, st *status) {
	button.Disable()
	st.set("Спрашиваю GitHub…", widget.LowImportance)
	cur := u.current()
	u.checkUpdates(func(out updateOutcome) {
		button.Enable()
		msg, importance := out.message(cur)
		st.set(msg, importance)
	})
}
