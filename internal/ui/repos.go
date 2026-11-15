package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
)

// showRepoPicker lists the account's GitHub repositories and puts the
// chosen one into the task form. When the saved token is missing or GitHub
// refuses it, the sign-in window comes first and the list opens after it.
func (u *UI) showRepoPicker() {
	if u.cfg.Token() == "" {
		u.showAuth("Чтобы показать ваши репозитории, войдите в GitHub.", u.showRepoPicker)
		return
	}

	var (
		dlg     *dialog.CustomDialog
		all     []ghapi.RepoSummary
		shown   []ghapi.RepoSummary
		closed  bool
		loadSeq int
	)
	ctx, cancel := context.WithCancel(context.Background())

	search := widget.NewEntry()
	search.SetPlaceHolder("Поиск по названию или описанию")
	search.Disable()

	list := widget.NewList(
		func() int { return len(shown) },
		func() fyne.CanvasObject { return newRepoRow() },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i < len(shown) {
				o.(*repoRow).set(shown[i])
			}
		},
	)
	list.OnSelected = func(i widget.ListItemID) {
		if i >= len(shown) {
			return
		}
		u.pickRepo(shown[i])
		dlg.Hide()
	}

	// Exactly one of these fills the middle of the window.
	spinner := widget.NewActivity()
	loading := container.NewCenter(container.NewHBox(fixed(22, 22, spinner), widget.NewLabel("Загружаю список репозиториев…")))
	noteTitle := text("", 15, colText, fyne.TextStyle{Bold: true})
	noteTitle.Alignment = fyne.TextAlignCenter
	noteBody := widget.NewLabel("")
	noteBody.Importance = widget.LowImportance
	noteBody.Alignment = fyne.TextAlignCenter
	noteBody.Wrapping = fyne.TextWrapWord
	var load func()
	retry := widget.NewButtonWithIcon("Повторить", theme.ViewRefreshIcon(), func() { load() })
	note := container.NewCenter(fixed(420, 150, container.NewVBox(noteTitle, noteBody, container.NewHBox(layout.NewSpacer(), retry, layout.NewSpacer()))))
	middle := container.NewStack(loading, note, list)
	show := func(o fyne.CanvasObject) {
		for _, c := range middle.Objects {
			c.Hide()
		}
		o.Show()
		if o == loading {
			spinner.Start()
		} else {
			spinner.Stop()
		}
	}
	say := func(title, body string, canRetry bool) {
		noteTitle.Text = title
		noteTitle.Refresh()
		noteBody.SetText(body)
		if canRetry {
			retry.Show()
		} else {
			retry.Hide()
		}
		show(note)
	}

	account := widget.NewLabel("")
	account.Importance = widget.LowImportance
	account.Truncation = fyne.TextTruncateEllipsis
	// limited explains why the list may be shorter than the account.
	limited := newStatus()

	filter := func() {
		query := strings.ToLower(strings.TrimSpace(search.Text))
		shown = shown[:0]
		for _, r := range all {
			if query == "" || strings.Contains(strings.ToLower(r.FullName), query) || strings.Contains(strings.ToLower(r.Description), query) {
				shown = append(shown, r)
			}
		}
		list.UnselectAll()
		list.Refresh()
		list.ScrollToTop()
		switch {
		case len(all) == 0:
			say("Репозиториев пока нет", "Закройте это окно и впишите название в форму в виде владелец/название — репозиторий будет создан при запуске.", false)
		case len(shown) == 0:
			say("Ничего не найдено", "По запросу «"+strings.TrimSpace(search.Text)+"» репозиториев нет. Измените запрос.", false)
		default:
			show(list)
		}
	}
	search.OnChanged = func(string) { filter() }

	reauth := func(reason string) {
		closed = true
		cancel()
		dlg.Hide()
		u.showAuth(reason, u.showRepoPicker)
	}

	load = func() {
		loadSeq++
		seq := loadSeq
		search.Disable()
		limited.clear()
		show(loading)
		client := u.github(u.cfg.Token())
		u.spawn(func() {
			reqCtx, stop := context.WithTimeout(ctx, 60*time.Second)
			defer stop()
			user, err := client.CurrentUser(reqCtx)
			var repos []ghapi.RepoSummary
			var more bool
			if err == nil {
				repos, more, err = client.ListRepos(reqCtx)
			}
			u.do(func() {
				if closed || seq != loadSeq {
					return
				}
				if reason := authReason(err); reason != "" {
					reauth(reason)
					return
				}
				if err != nil {
					say("Список не загрузился", capitalize(err.Error())+".", true)
					return
				}
				all = repos
				count := fmt.Sprintf("%d %s", len(repos), pluralRu(len(repos), "репозиторий", "репозитория", "репозиториев"))
				if more {
					count = fmt.Sprintf("последние %d", len(repos))
				}
				account.SetText(user.Login + " · " + count)
				switch {
				case user.Classic && !user.HasScope("repo"):
					limited.set("Видны только публичные репозитории: у токена нет области repo. Чтобы увидеть приватные, войдите заново.", widget.WarningImportance)
				case !user.Classic:
					limited.set("Видны репозитории, к которым у токена есть доступ. Если нужного нет — войдите с другим токеном.", widget.LowImportance)
				}
				search.Enable()
				filter()
				u.win.Canvas().Focus(search)
			})
		})
	}

	refresh := iconButton(theme.ViewRefreshIcon(), func() { load() })
	relogin := quietButton("Сменить аккаунт", theme.AccountIcon(), func() {
		reauth("Войдите под другим аккаунтом или с другим токеном.")
	})
	closeBtn := widget.NewButton("Закрыть", func() { dlg.Hide() })

	top := container.NewBorder(nil, nil, nil, refresh, search)
	bottom := container.NewVBox(
		limited.label,
		container.NewBorder(nil, nil, nil, container.NewHBox(relogin, closeBtn), account),
	)
	content := container.NewBorder(top, bottom, nil, nil, container.New(layout.NewCustomPaddedLayout(6, 6, 0, 0), middle))
	// The note under the list takes its height from the list.
	limited.after = func() {
		bottom.Refresh()
		content.Refresh()
	}
	dlg = dialog.NewCustomWithoutButtons("Репозитории GitHub", content, u.win)
	dlg.SetOnClosed(func() {
		closed = true
		cancel()
	})
	size := u.win.Canvas().Size()
	dlg.Resize(fyne.NewSize(min(640, size.Width-40), min(600, size.Height-40)))
	dlg.Show()
	u.repoDialog = dlg
	load()
}

// pickRepo puts a repository from the list into the task form.
func (u *UI) pickRepo(r ghapi.RepoSummary) {
	u.repo.SetText(r.FullName)
	// A branch typed earlier belonged to another repository. Empty means
	// the default one, named in the placeholder.
	u.branch.SetText("")
	if r.DefaultBranch != "" {
		u.branch.SetPlaceHolder(r.DefaultBranch)
	} else {
		u.branch.SetPlaceHolder("основная")
	}
	if !r.Writable() {
		why := "у токена нет права записи в него"
		if r.Archived {
			why = "он в архиве"
		}
		u.showProblem("В " + r.FullName + " записать код не получится: " + why + ".")
	}
}

// repoRow is one line of the repository list.
type repoRow struct {
	widget.BaseWidget
	name, about, access, pushed *widget.Label
	body                        *fyne.Container
}

func newRepoRow() *repoRow {
	r := &repoRow{}
	r.name = widget.NewLabel("")
	r.name.TextStyle = fyne.TextStyle{Bold: true}
	r.name.Truncation = fyne.TextTruncateEllipsis
	r.about = widget.NewLabel("")
	r.about.Importance = widget.LowImportance
	r.about.Truncation = fyne.TextTruncateEllipsis
	r.about.SizeName = theme.SizeNameCaptionText
	r.access = widget.NewLabel("")
	r.access.SizeName = theme.SizeNameCaptionText
	r.pushed = widget.NewLabel("")
	r.pushed.Importance = widget.LowImportance
	r.pushed.SizeName = theme.SizeNameCaptionText
	r.body = container.New(layout.NewCustomPaddedLayout(-2, -2, 0, 0), container.NewVBox(
		container.NewBorder(nil, nil, nil, r.access, r.name),
		container.New(layout.NewCustomPaddedLayout(-10, 0, 0, 0), container.NewBorder(nil, nil, nil, r.pushed, r.about)),
	))
	r.ExtendBaseWidget(r)
	return r
}

func (r *repoRow) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(r.body) }

func (r *repoRow) set(repo ghapi.RepoSummary) {
	r.name.SetText(repo.FullName)

	about := strings.TrimSpace(repo.Description)
	if about == "" {
		about = "Без описания"
	}
	if repo.Language != "" {
		about = repo.Language + " · " + about
	}
	r.about.SetText(about)

	switch {
	case repo.Archived:
		r.access.Importance = widget.WarningImportance
		r.access.SetText("архив")
	case !repo.Permissions.Push:
		r.access.Importance = widget.WarningImportance
		r.access.SetText("только чтение")
	case repo.Private:
		r.access.Importance = widget.LowImportance
		r.access.SetText("приватный")
	default:
		r.access.Importance = widget.LowImportance
		r.access.SetText("публичный")
	}

	if repo.PushedAt.IsZero() {
		r.pushed.SetText("пусто")
	} else {
		r.pushed.SetText(ago(time.Since(repo.PushedAt)))
	}
	// The labels on the right changed width: give the columns their room.
	r.body.Refresh()
}

// ago words how long ago something happened.
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "только что"
	case d < 24*time.Hour:
		n := int(d.Hours())
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "час", "часа", "часов"))
	case d < 31*24*time.Hour:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "день", "дня", "дней"))
	case d < 365*24*time.Hour:
		n := int(d.Hours() / 24 / 30)
		return fmt.Sprintf("%d %s назад", n, pluralRu(n, "месяц", "месяца", "месяцев"))
	}
	n := int(d.Hours() / 24 / 365)
	return fmt.Sprintf("%d %s назад", n, pluralRu(n, "год", "года", "лет"))
}
