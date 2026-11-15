package ui

import (
	"context"
	"errors"
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

// github makes a client for the given token. Tests point it at a stand-in
// server through ghBaseURL.
func (u *UI) github(token string) *ghapi.Client {
	return &ghapi.Client{Token: token, BaseURL: u.ghBaseURL}
}

// showAuth opens the GitHub sign-in window. reason says why it appeared;
// done, when not nil, runs after a successful sign-in.
//
// There are two ways in. Through the browser (GitHub's device flow): the
// window shows a short code, the user confirms it on github.com. That needs
// the Client ID of an OAuth App, set in the settings. Or with an access
// token pasted into the field.
func (u *UI) showAuth(reason string, done func()) {
	var dlg *dialog.CustomDialog
	ctx, cancel := context.WithCancel(context.Background())
	closed := false

	// accept stores a token that GitHub has just confirmed.
	accept := func(token string) error {
		next := u.cfg
		next.GitHubToken = token
		if err := u.Save(next); err != nil {
			return err
		}
		u.cfg = next
		closed = true
		cancel()
		dlg.Hide()
		u.clearProblem()
		u.CheckConnections()
		if done != nil {
			done()
		}
		return nil
	}

	relayout := func() {}

	// --- with a token ----------------------------------------------------
	token := widget.NewPasswordEntry()
	token.SetPlaceHolder("ghp_… или github_pat_…")
	tokenStatus := newStatus()
	tokenStatus.after = func() { relayout() }
	var signIn *widget.Button
	check := func() {
		value := strings.TrimSpace(token.Text)
		if value == "" {
			tokenStatus.set("Вставьте токен в поле выше.", widget.DangerImportance)
			return
		}
		signIn.Disable()
		tokenStatus.set("Проверяю токен…", widget.LowImportance)
		client := u.github(value)
		u.spawn(func() {
			reqCtx, stop := context.WithTimeout(ctx, 20*time.Second)
			defer stop()
			_, err := client.CurrentUser(reqCtx)
			u.do(func() {
				if closed {
					return
				}
				signIn.Enable()
				var api *ghapi.APIError
				if errors.As(err, &api) && api.Status == 401 {
					tokenStatus.set("GitHub не принял этот токен. Проверьте, что он скопирован целиком и не истёк.", widget.DangerImportance)
					return
				}
				if err != nil {
					tokenStatus.set(capitalize(err.Error()), widget.DangerImportance)
					return
				}
				if err := accept(value); err != nil {
					tokenStatus.set(capitalize(err.Error()), widget.DangerImportance)
				}
			})
		})
	}
	signIn = widget.NewButtonWithIcon("Войти с токеном", theme.LoginIcon(), check)
	token.OnSubmitted = func(string) { check() }
	createToken := widget.NewHyperlink("Создать токен на GitHub", mustURL(ghapi.TokenURL()))
	tokenPart := container.NewVBox(
		sectionTitle("Токен доступа"),
		hint("Classic-токену нужны области repo и workflow — ссылка ниже откроет страницу, где они уже отмечены. Fine-grained токену: Contents и Workflows — Read and write, Actions — Read."),
		token,
		container.NewHBox(signIn, createToken),
		tokenStatus.label,
	)

	// --- through the browser ---------------------------------------------
	var browserPart fyne.CanvasObject
	if clientID := u.cfg.GitHubClientID; clientID != "" {
		flow := &ghapi.DeviceFlow{ClientID: clientID, BaseURL: u.oauthBaseURL, Sleep: u.deviceSleep}
		code := text("", 30, colEmerald, fyne.TextStyle{Monospace: true, Bold: true})
		code.Alignment = fyne.TextAlignCenter
		steps := hint("")
		spinner := widget.NewActivity()
		waiting := widget.NewLabel("Жду подтверждения на github.com…")
		waiting.Importance = widget.LowImportance
		browserStatus := newStatus()
		browserStatus.after = func() { relayout() }
		var openPage, copyCode, start *widget.Button
		pageURL := ""
		openPage = widget.NewButtonWithIcon("Открыть страницу GitHub", theme.ComputerIcon(), func() { u.openURL(pageURL) })
		copyCode = widget.NewButtonWithIcon("Скопировать код", theme.ContentCopyIcon(), func() { u.copyText(code.Text) })
		codeBox := container.NewVBox(
			steps,
			container.New(layout.NewCustomPaddedLayout(6, 6, 0, 0), code),
			container.NewHBox(layout.NewSpacer(), copyCode, openPage, layout.NewSpacer()),
			container.NewHBox(layout.NewSpacer(), fixed(22, 22, spinner), waiting, layout.NewSpacer()),
		)
		codeBox.Hide()

		start = widget.NewButtonWithIcon("Войти через GitHub", theme.LoginIcon(), func() {
			start.Disable()
			browserStatus.set("Запрашиваю код у GitHub…", widget.LowImportance)
			u.spawn(func() {
				reqCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				dc, err := flow.Start(reqCtx)
				stop()
				u.do(func() {
					if closed {
						return
					}
					if err != nil {
						start.Enable()
						browserStatus.set(capitalize(err.Error()), widget.DangerImportance)
						return
					}
					pageURL = dc.VerificationURL
					code.Text = dc.UserCode
					code.Refresh()
					steps.SetText("Откройте " + strings.TrimPrefix(dc.VerificationURL, "https://") + ", введите этот код и подтвердите доступ для Kolibri Forge. Окно закроется само.")
					start.Hide()
					codeBox.Show()
					browserStatus.clear()
					spinner.Start()
					u.openURL(pageURL)
				})
				if err != nil {
					return
				}
				value, err := flow.Wait(ctx, dc)
				u.do(func() {
					if closed {
						return
					}
					spinner.Stop()
					if err == nil {
						err = accept(value)
					}
					if err != nil {
						codeBox.Hide()
						start.SetText("Запросить новый код")
						start.Show()
						start.Enable()
						browserStatus.set(capitalize(err.Error())+".", widget.DangerImportance)
					}
				})
			})
		})
		start.Importance = widget.HighImportance
		browserPart = container.NewVBox(
			sectionTitle("Через браузер"),
			hint("Без токена: GitHub покажет страницу подтверждения, а программа получит доступ сама."),
			container.NewHBox(start),
			codeBox,
			browserStatus.label,
		)
	} else {
		signIn.Importance = widget.HighImportance
	}

	body := container.NewVBox()
	// Parts of the window appear and disappear; the column is laid out
	// again each time.
	relayout = body.Refresh
	if reason != "" {
		why := widget.NewLabel(reason)
		why.Wrapping = fyne.TextWrapWord
		why.Importance = widget.WarningImportance
		body.Add(why)
	}
	if browserPart != nil {
		body.Add(browserPart)
	}
	body.Add(tokenPart)
	if browserPart == nil {
		body.Add(hint("Вход через браузер, без токена, включается в «Настройках»: укажите там Client ID своего OAuth-приложения GitHub."))
	}

	cancelBtn := widget.NewButton("Отмена", func() { dlg.Hide() })
	buttons := container.New(layout.NewCustomPaddedLayout(8, 0, 0, 0), container.NewHBox(layout.NewSpacer(), cancelBtn))
	content := container.NewBorder(nil, buttons, nil, nil,
		container.NewVScroll(container.New(layout.NewCustomPaddedLayout(0, 0, 4, 14), body)))
	dlg = dialog.NewCustomWithoutButtons("Вход в GitHub", content, u.win)
	dlg.SetOnClosed(func() {
		closed = true
		cancel()
	})
	size := u.win.Canvas().Size()
	height := float32(490)
	if browserPart != nil {
		height = 690
	}
	dlg.Resize(fyne.NewSize(min(560, size.Width-40), min(height, size.Height-40)))
	dlg.Show()
	u.authDialog = dlg
}

// authReason words a GitHub failure for the sign-in window, or returns ""
// when signing in again would not help.
func authReason(err error) string {
	var api *ghapi.APIError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &api) && api.Status == 401:
		return "GitHub не принял сохранённый токен: он истёк или отозван. Войдите заново."
	case ghapi.IsAuthError(err):
		return "Текущему токену GitHub не разрешил это действие: " + api.Message + ". Войдите с токеном, у которого есть нужные права."
	}
	return ""
}
