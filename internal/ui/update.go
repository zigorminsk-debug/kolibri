package ui

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/update"
)

// current says which build is running. The numbers are stamped into the
// binary by the packaging step; a program built from source has none.
func (u *UI) current() update.Current {
	if u.version != nil {
		return *u.version
	}
	meta := u.app.Metadata()
	return update.Current{Version: meta.Version, Build: meta.Build}
}

// updateOutcome is what a check for updates found.
type updateOutcome struct {
	found update.Found
	ok    bool // a newer release exists
	err   error
}

// message words the outcome for the "check now" buttons.
func (o updateOutcome) message(cur update.Current) (string, widget.Importance) {
	switch {
	case o.err != nil:
		return "Не удалось проверить: " + o.err.Error(), widget.DangerImportance
	case o.ok && !cur.Known():
		return "Последний релиз на GitHub — " + o.found.Title + ". Эта программа собрана из исходников, сравнить версии нельзя.", widget.MediumImportance
	case o.ok:
		return "Доступно обновление: " + o.found.Title + ".", widget.WarningImportance
	case !cur.Known():
		return "Релизов на GitHub пока нет.", widget.MediumImportance
	}
	return "У вас последняя версия.", widget.SuccessImportance
}

// checkUpdates asks GitHub for the releases of the program. The request
// carries no token: the repository is public, and a token that GitHub has
// stopped accepting must not break the check.
//
// then, when not nil, gets the outcome (a manual check). The start-up check
// passes nil: it stays silent unless there is something to offer, and does
// not repeat an offer the user has declined.
func (u *UI) checkUpdates(then func(updateOutcome)) {
	client := &ghapi.Client{BaseURL: u.ghBaseURL}
	cur := u.current()
	builds := u.cfg.UpdateBuilds
	skipped := u.cfg.UpdateSkipped
	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		releases, err := client.Releases(ctx, update.Owner, update.Repo, 30)
		var out updateOutcome
		switch {
		case ghapi.IsNotFound(err):
			// No such repository for this caller: the same as no releases.
		case err != nil:
			out.err = err
		default:
			// A program built from source has no number to compare: for
			// it "newer" simply means the latest release of any kind.
			out.found, out.ok = update.Pick(releases, cur, builds || !cur.Known())
		}
		u.do(func() {
			if out.err != nil {
				u.Log.Line(time.Now(), "!", "Проверка обновлений: "+out.err.Error())
			}
			manual := then != nil
			if out.ok && cur.Known() && (manual || out.found.Release.TagName != skipped) {
				u.showUpdateBar(out.found)
			}
			if manual {
				then(out)
			}
		})
	})
}

// checkUpdatesAtStart is the quiet check made once when the program opens.
// A program built from source has no version to compare and does not ask.
func (u *UI) checkUpdatesAtStart() {
	if u.cfg.CheckUpdates && u.current().Known() {
		u.checkUpdates(nil)
	}
}

// updateBarHolder is the strip under the header; it is empty until a newer
// release is found.
func (u *UI) updateBarHolder() fyne.CanvasObject {
	u.updateBar = container.NewStack()
	u.updateBar.Hide()
	return u.updateBar
}

func (u *UI) showUpdateBar(found update.Found) {
	if u.updateBar == nil {
		return
	}
	u.updateFound = &found
	bg := canvas.NewRectangle(withAlpha(colEmerald, 0x1C))
	bg.CornerRadius = 10
	bg.StrokeColor = withAlpha(colEmerald, 0x90)
	bg.StrokeWidth = 1

	icon := widget.NewIcon(theme.DownloadIcon())
	label := widget.NewLabel("Доступно обновление " + AppName + ": " + found.Title)
	label.TextStyle = fyne.TextStyle{Bold: true}
	label.Truncation = fyne.TextTruncateEllipsis
	more := quietButton("Подробнее", nil, func() { u.showUpdate(found) })
	get := widget.NewButtonWithIcon("Скачать", theme.DownloadIcon(), func() { u.downloadUpdate(found) })
	get.Importance = widget.HighImportance
	closeBtn := iconButton(theme.CancelIcon(), u.hideUpdateBar)

	row := container.NewBorder(nil, nil, container.NewCenter(icon), container.NewHBox(more, get, closeBtn), label)
	u.updateBar.Objects = []fyne.CanvasObject{bg, container.New(layout.NewCustomPaddedLayout(4, 4, 12, 6), row)}
	u.updateBar.Show()
	u.relayoutRoot()
}

func (u *UI) hideUpdateBar() {
	if u.updateBar == nil || !u.updateBar.Visible() {
		return
	}
	u.updateBar.Hide()
	u.relayoutRoot()
}

// relayoutRoot makes room for the update strip (or takes it back).
func (u *UI) relayoutRoot() {
	if u.top != nil {
		u.top.Refresh()
	}
	if u.root != nil {
		u.root.Refresh()
	}
}

// updateTarget is where "download" leads: the file for this system, or the
// release page when the release has no such file.
func updateTarget(found update.Found, goos string) (link, label string) {
	if asset, ok := update.Asset(found.Release, goos); ok {
		if goos == "windows" {
			return asset.URL, "Скачать установщик"
		}
		return asset.URL, "Скачать архив"
	}
	return found.Release.HTMLURL, "Открыть страницу релиза"
}

func (u *UI) downloadUpdate(found update.Found) {
	link, _ := updateTarget(found, u.goos())
	u.openURL(link)
}

func (u *UI) goos() string {
	if u.goosOverride != "" {
		return u.goosOverride
	}
	return runtime.GOOS
}

// showUpdate tells what the newer release is and lets the user take it,
// postpone it or decline it for good.
func (u *UI) showUpdate(found update.Found) {
	have := "У вас: " + u.versionLine() + "."
	if found.Build {
		have += " Это промежуточная сборка: она свежее, но проверена меньше, чем полноценная версия."
	}
	notes := found.Release.Body
	if len([]rune(notes)) > 6000 {
		notes = string([]rune(notes)[:6000]) + "…"
	}
	if notes == "" {
		notes = "Описание к этому релизу не приложено."
	}
	body := widget.NewRichTextFromMarkdown(notes)
	body.Wrapping = fyne.TextWrapWord

	how := "Linux и macOS: распакуйте скачанный архив поверх старой версии."
	if u.goos() == "windows" {
		how = "Запустите скачанный установщик: он закроет программу и обновит её, настройки и журнал сохранятся."
	}
	published := ""
	if !found.Release.PublishedAt.IsZero() {
		published = "Опубликовано " + found.Release.PublishedAt.Local().Format("02.01.2006") + ". "
	}

	var dlg *dialog.CustomDialog
	_, getLabel := updateTarget(found, u.goos())
	get := widget.NewButtonWithIcon(getLabel, theme.DownloadIcon(), func() { u.downloadUpdate(found) })
	get.Importance = widget.HighImportance
	skip := widget.NewButton("Пропустить эту версию", func() {
		next := u.cfg
		next.UpdateSkipped = found.Release.TagName
		if err := u.Save(next); err == nil {
			u.cfg = next
		}
		u.hideUpdateBar()
		dlg.Hide()
	})
	later := widget.NewButton("Позже", func() { dlg.Hide() })
	page := widget.NewHyperlink("Страница релиза на GitHub", mustURL(found.Release.HTMLURL))

	head := container.NewVBox(
		text(fmt.Sprintf("%s — %s", AppName, found.Title), 17, colText, fyne.TextStyle{Bold: true}),
		hint(published+have),
		widget.NewSeparator(),
	)
	foot := container.NewVBox(
		widget.NewSeparator(),
		hint(how),
		container.NewHBox(page, layout.NewSpacer(), skip, later, get),
	)
	content := container.NewBorder(head, foot, nil, nil, container.NewVScroll(body))
	dlg = dialog.NewCustomWithoutButtons("Обновление", content, u.win)
	u.updateDialog = dlg
	size := u.win.Canvas().Size()
	dlg.Resize(fyne.NewSize(min(680, size.Width-40), min(560, size.Height-40)))
	dlg.Show()
}
