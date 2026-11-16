package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
)

const (
	autoModel = "Первая на сервере"
)

func sectionTitle(s string) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(10, 2, 0, 0), text(s, 15, colText, fyne.TextStyle{Bold: true}))
}

// status is a one-line result of a "check" button. It appears and changes
// height as it is set, so the layout around it has to be redone: after does
// that.
type status struct {
	label *widget.Label
	after func()
}

func newStatus() *status {
	l := widget.NewLabel("")
	l.Wrapping = fyne.TextWrapWord
	l.Hide()
	return &status{label: l}
}

func (s *status) set(msg string, importance widget.Importance) {
	s.label.Importance = importance
	s.label.SetText(msg)
	s.label.Show()
	if s.after != nil {
		s.after()
	}
}

func (s *status) clear() {
	s.label.Hide()
	if s.after != nil {
		s.after()
	}
}

func (u *UI) showSettings() {
	cfg := u.cfg

	address := widget.NewEntry()
	address.SetText(cfg.ColibriURL)
	address.SetPlaceHolder("http://127.0.0.1:8000")
	llmKey := widget.NewPasswordEntry()
	llmKey.SetText(cfg.ColibriKey)
	llmKey.SetPlaceHolder("не нужен, если сервер запущен без COLI_API_KEY")

	modelOptions := func(models []colibri.Model) []string {
		opts := []string{autoModel}
		seen := false
		for _, m := range models {
			opts = append(opts, m.ID)
			seen = seen || m.ID == cfg.Model
		}
		if cfg.Model != "" && !seen {
			opts = append(opts, cfg.Model)
		}
		return opts
	}
	model := widget.NewSelect(modelOptions(u.models), nil)
	if cfg.Model == "" {
		model.SetSelected(autoModel)
	} else {
		model.SetSelected(cfg.Model)
	}
	llmStatus := newStatus()
	var checkLLM *widget.Button
	checkLLM = widget.NewButtonWithIcon("Проверить и обновить модели", theme.ViewRefreshIcon(), func() {
		checkLLM.Disable()
		llmStatus.set("Спрашиваю сервер…", widget.LowImportance)
		client := &colibri.Client{BaseURL: address.Text, APIKey: strings.TrimSpace(llmKey.Text)}
		u.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			models, err := client.Models(ctx)
			u.do(func() {
				checkLLM.Enable()
				if err != nil {
					llmStatus.set(capitalize(err.Error()), widget.DangerImportance)
					return
				}
				if len(models) == 0 {
					llmStatus.set("Сервер отвечает, но модель не загружена.", widget.WarningImportance)
					return
				}
				current := model.Selected
				model.Options = modelOptions(models)
				model.SetSelected(current)
				names := make([]string, len(models))
				for i, m := range models {
					names[i] = m.ID
					var meta []string
					if m.ContextLength > 0 {
						meta = append(meta, fmt.Sprintf("контекст %d", m.ContextLength))
					}
					if m.Vision {
						meta = append(meta, "скриншоты")
					}
					if len(meta) > 0 {
						names[i] += " (" + strings.Join(meta, ", ") + ")"
					}
				}
				llmStatus.set("Колибри на связи: "+strings.Join(names, ", "), widget.SuccessImportance)
			})
		})
	})

	temperature := widget.NewEntry()
	temperature.SetText(strconv.FormatFloat(cfg.Temperature, 'f', -1, 64))
	maxTokens := widget.NewEntry()
	maxTokens.SetText(strconv.Itoa(cfg.MaxTokens))
	idle := widget.NewEntry()
	idle.SetText(strconv.Itoa(cfg.IdleTimeoutMin))

	token := widget.NewPasswordEntry()
	token.SetText(cfg.GitHubToken)
	if cfg.GitHubToken == "" && cfg.Token() != "" {
		token.SetPlaceHolder("используется токен из переменной окружения")
	} else {
		token.SetPlaceHolder("ghp_… или github_pat_…")
	}
	ghStatus := newStatus()
	var checkGH *widget.Button
	checkGH = widget.NewButtonWithIcon("Проверить токен", theme.ConfirmIcon(), func() {
		value := strings.TrimSpace(token.Text)
		if value == "" {
			value = cfg.Token()
		}
		if value == "" {
			ghStatus.set("Вставьте токен.", widget.DangerImportance)
			return
		}
		checkGH.Disable()
		ghStatus.set("Спрашиваю GitHub…", widget.LowImportance)
		client := u.github(value)
		u.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			user, err := client.CurrentUser(ctx)
			u.do(func() {
				checkGH.Enable()
				switch {
				case err != nil:
					ghStatus.set(capitalize(err.Error()), widget.DangerImportance)
				case !user.Classic:
					ghStatus.set("Токен принят, аккаунт "+user.Login+". Это fine-grained токен: убедитесь, что у него есть Contents и Workflows — Read and write, и Actions — Read.", widget.SuccessImportance)
				case !user.HasScope("repo") && !user.HasScope("public_repo"):
					ghStatus.set("Токен принят, аккаунт "+user.Login+", но нет области repo — записать код не получится.", widget.WarningImportance)
				case !user.HasScope("workflow"):
					ghStatus.set("Токен принят, аккаунт "+user.Login+", но нет области workflow — GitHub не даст записать файл сборки.", widget.WarningImportance)
				default:
					ghStatus.set("Токен принят, аккаунт "+user.Login+". Права repo и workflow есть.", widget.SuccessImportance)
				}
			})
		})
	})
	tokenLink := widget.NewHyperlink("Создать токен с нужными правами", mustURL(ghapi.TokenURL()))
	clientID := widget.NewEntry()
	clientID.SetText(cfg.GitHubClientID)
	clientID.SetPlaceHolder("Ov23li… — не обязательно")

	outDir := widget.NewEntry()
	outDir.SetText(cfg.OutputDir)
	pick := widget.NewButtonWithIcon("Выбрать…", theme.FolderOpenIcon(), func() {
		dialog.ShowFolderOpen(func(dir fyne.ListableURI, err error) {
			if err == nil && dir != nil {
				outDir.SetText(dir.Path())
			}
		}, u.win)
	})

	// Lightpanda browser settings.
	lpPath := widget.NewEntry()
	lpPath.SetText(cfg.LightpandaPath)
	lpPath.SetPlaceHolder("автоматически: папка приложения, PATH, WSL2 или Docker")
	lpURL := widget.NewEntry()
	lpURL.SetText(cfg.LightpandaURL)
	lpURL.SetPlaceHolder("http://127.0.0.1:9222 — не обязательно (lightpanda mcp --port 9222)")
	lpProxy := widget.NewEntry()
	lpProxy.SetText(cfg.LightpandaProxy)
	lpProxy.SetPlaceHolder("http://proxy:8080 — не обязательно")
	lpRobots := widget.NewCheck("Учитывать robots.txt при открытии сайтов", nil)
	lpRobots.SetChecked(cfg.LightpandaRobots)

	searchProv := widget.NewSelect(searchProviderOptions(), nil)
	searchProv.SetSelected(searchProviderTitle(cfg.SearchProvider))
	searchKey := widget.NewPasswordEntry()
	searchKey.SetText(cfg.SearchAPIKey)
	searchKey.SetPlaceHolder("не нужен для автоматического поиска")

	webSteps := widget.NewEntry()
	webSteps.SetText(strconv.Itoa(cfg.MaxWebSteps))
	webTimeout := widget.NewEntry()
	webTimeout.SetText(strconv.Itoa(cfg.WebTimeoutSec))

	draftBrowser := func() *lightpanda.Browser {
		tmp := config.Config{
			LightpandaPath:   strings.TrimSpace(lpPath.Text),
			LightpandaURL:    strings.TrimSpace(lpURL.Text),
			LightpandaProxy:  strings.TrimSpace(lpProxy.Text),
			LightpandaRobots: lpRobots.Checked,
			SearchProvider:   searchProviderID(searchProv.Selected),
			SearchAPIKey:     strings.TrimSpace(searchKey.Text),
			WebTimeoutSec:    cfg.WebTimeoutSec,
		}
		return u.browser(tmp)
	}
	lpStatus := newStatus()
	var checkLP, installLP *widget.Button
	checkLP = widget.NewButtonWithIcon("Проверить Lightpanda", theme.ConfirmIcon(), func() {
		u.checkLightpandaInto(draftBrowser(), checkLP, lpStatus)
	})
	installLP = widget.NewButtonWithIcon("Установить Lightpanda", theme.DownloadIcon(), func() {
		u.installLightpandaInto(draftBrowser(), installLP, checkLP, lpStatus, nil)
	})
	lpLink := widget.NewHyperlink("lightpanda-io/browser", mustURL("https://github.com/lightpanda-io/browser"))

	autoUpdate := widget.NewCheck("Проверять обновления при запуске", nil)
	autoUpdate.SetChecked(cfg.CheckUpdates)
	updateBuilds := widget.NewCheck("Предлагать промежуточные сборки", nil)
	updateBuilds.SetChecked(cfg.UpdateBuilds)
	updateStatus := newStatus()
	var checkUpdate *widget.Button
	checkUpdate = widget.NewButtonWithIcon("Проверить обновления", theme.ViewRefreshIcon(), func() {
		u.checkUpdatesInto(checkUpdate, updateStatus)
	})

	problem := newStatus()
	row := func(label string, control fyne.CanvasObject) fyne.CanvasObject {
		return container.NewVBox(fieldLabel(label), control)
	}
	three := container.NewGridWithColumns(3,
		row("Температура", temperature),
		row("Макс. токенов ответа", maxTokens),
		row("Тишина до обрыва, мин", idle),
	)
	form := container.NewVBox(
		sectionTitle("Колибри"),
		row("Адрес сервера", address),
		row("API-ключ", llmKey),
		row("Модель", model),
		container.NewHBox(checkLLM),
		llmStatus.label,

		sectionTitle("GitHub"),
		row("Токен доступа", token),
		hint("Classic-токену нужны области repo и workflow. Fine-grained — Contents и Workflows: Read and write, Actions: Read. Токен хранится в файле настроек на этом компьютере."),
		container.NewHBox(checkGH, tokenLink),
		ghStatus.label,
		row("Client ID OAuth-приложения", clientID),
		hint("Нужен только для входа через браузер, без токена. Создайте OAuth App в GitHub → Settings → Developer settings, включите в нём «Enable Device Flow» и вставьте сюда его Client ID."),

		sectionTitle("Генерация"),
		three,
		hint("0 токенов — без ограничения: ответ может занять весь контекст модели. Колибри шлёт сигнал каждые 10 секунд; если сервер молчит дольше указанного, запрос обрывается."),

		sectionTitle("Браузер Lightpanda"),
		hint("На Windows сразу работает встроенный нативный движок Lightpanda (HTML5 DOM, CSS-селекторы, чтение в Markdown, формы, поисковые системы и безголовый JS-рендеринг через Edge/Chrome) — без WSL и Docker. При наличии внешнего Lightpanda (Linux/macOS, WSL2, Docker или MCP-сервера) используется он."),
		container.NewHBox(checkLP, installLP, lpLink),
		lpStatus.label,
		row("Путь к исполняемому файлу", lpPath),
		row("Адрес MCP-сервера Lightpanda", lpURL),
		container.NewGridWithColumns(2,
			row("Поисковая система", searchProv),
			row("API-ключ поиска", searchKey),
		),
		container.NewGridWithColumns(2,
			row("Макс. шагов в интернете", webSteps),
			row("Таймаут шага, сек", webTimeout),
		),
		row("HTTP-прокси для браузера", lpProxy),
		lpRobots,

		sectionTitle("Готовые сборки"),
		row("Папка, куда скачивать программы", container.NewBorder(nil, nil, nil, pick, outDir)),

		sectionTitle("Обновления"),
		autoUpdate,
		updateBuilds,
		hint("Программа спрашивает у GitHub список релизов и сравнивает со своей версией; токен при этом не используется. Сама она ничего не скачивает и не устанавливает. Промежуточные сборки выходят между версиями: они свежее, но проверены меньше."),
		container.NewHBox(checkUpdate),
		updateStatus.label,
		problem.label,
		gap(0, 4),
	)

	for _, st := range []*status{llmStatus, ghStatus, lpStatus, updateStatus, problem} {
		st.after = form.Refresh
	}

	var dlg *dialog.CustomDialog
	save := widget.NewButtonWithIcon("Сохранить", theme.DocumentSaveIcon(), func() {
		next := u.cfg
		next.ColibriURL = strings.TrimSpace(address.Text)
		if err := validAddress(next.ColibriURL); err != nil {
			problem.set(capitalize(err.Error()), widget.DangerImportance)
			return
		}
		next.ColibriKey = strings.TrimSpace(llmKey.Text)
		next.Model = model.Selected
		if next.Model == autoModel {
			next.Model = ""
		}
		t, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(temperature.Text), ",", "."), 64)
		if err != nil || t < 0 || t > 2 {
			problem.set("Температура — число от 0 до 2, например 0.2.", widget.DangerImportance)
			return
		}
		next.Temperature = t
		n, err := strconv.Atoi(strings.TrimSpace(maxTokens.Text))
		if err != nil || n < 0 {
			problem.set("Макс. токенов — целое число, 0 означает «без ограничения».", widget.DangerImportance)
			return
		}
		next.MaxTokens = n
		m, err := strconv.Atoi(strings.TrimSpace(idle.Text))
		if err != nil || m < 1 {
			problem.set("Время тишины — целое число минут, не меньше 1.", widget.DangerImportance)
			return
		}
		next.IdleTimeoutMin = m
		ws, err := strconv.Atoi(strings.TrimSpace(webSteps.Text))
		if err != nil || ws < 1 || ws > 20 {
			problem.set("Макс. шагов в интернете — целое число от 1 до 20.", widget.DangerImportance)
			return
		}
		next.MaxWebSteps = ws
		wt, err := strconv.Atoi(strings.TrimSpace(webTimeout.Text))
		if err != nil || wt < 5 || wt > 180 {
			problem.set("Таймаут шага в интернете — целое число секунд от 5 до 180.", widget.DangerImportance)
			return
		}
		next.WebTimeoutSec = wt
		next.LightpandaPath = strings.TrimSpace(lpPath.Text)
		next.LightpandaURL = strings.TrimSpace(lpURL.Text)
		if next.LightpandaURL != "" {
			if _, err := lightpanda.NormalizeMCPURL(next.LightpandaURL); err != nil {
				problem.set(capitalize(err.Error()), widget.DangerImportance)
				return
			}
		}
		next.LightpandaProxy = strings.TrimSpace(lpProxy.Text)
		if next.LightpandaProxy != "" {
			if p, err := url.Parse(next.LightpandaProxy); err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https") {
				problem.set("HTTP-прокси должен выглядеть как http://proxy:8080.", widget.DangerImportance)
				return
			}
		}
		next.LightpandaRobots = lpRobots.Checked
		next.SearchProvider = searchProviderID(searchProv.Selected)
		next.SearchAPIKey = strings.TrimSpace(searchKey.Text)
		next.GitHubToken = strings.TrimSpace(token.Text)
		next.GitHubClientID = strings.TrimSpace(clientID.Text)
		next.CheckUpdates, next.UpdateBuilds = autoUpdate.Checked, updateBuilds.Checked
		if dir := strings.TrimSpace(outDir.Text); dir != "" {
			next.OutputDir = dir
		}

		u.formToConfig()
		next.Repo, next.Branch, next.ProjectKind, next.Targets = u.cfg.Repo, u.cfg.Branch, u.cfg.ProjectKind, u.cfg.Targets
		next.LastTask, next.MaxFixes, next.UseWeb, next.Private, next.Release = u.cfg.LastTask, u.cfg.MaxFixes, u.cfg.UseWeb, u.cfg.Private, u.cfg.Release
		if err := u.Save(next); err != nil {
			problem.set(capitalize(err.Error()), widget.DangerImportance)
			return
		}
		u.cfg = next
		u.clearProblem()
		dlg.Hide()
		u.CheckConnections()
	})
	save.Importance = widget.HighImportance
	cancel := widget.NewButton("Отмена", func() { dlg.Hide() })

	scroll := container.NewVScroll(container.New(layout.NewCustomPaddedLayout(0, 0, 4, 14), form))
	buttons := container.New(layout.NewCustomPaddedLayout(8, 0, 0, 0),
		container.NewHBox(container.NewCenter(text(u.versionLine(), 12, colFaint, fyne.TextStyle{})), layout.NewSpacer(), cancel, save))
	content := container.NewBorder(nil, buttons, nil, nil, scroll)
	dlg = dialog.NewCustomWithoutButtons("Настройки", content, u.win)
	size := u.win.Canvas().Size()
	dlg.Resize(fyne.NewSize(min(640, size.Width-40), min(720, size.Height-40)))
	dlg.Show()
}

// versionLine names the build the user is running; the numbers are stamped
// into the binary by the packaging step on GitHub Actions.
func (u *UI) versionLine() string {
	cur := u.current()
	switch {
	case !cur.Known():
		return AppName + " · сборка из исходников"
	case cur.Build > 0:
		return fmt.Sprintf("%s %s · сборка %d", AppName, cur.Version, cur.Build)
	default:
		return AppName + " " + cur.Version
	}
}

func validAddress(s string) error {
	parsed, err := url.Parse(s)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("адрес Колибри должен выглядеть как http://127.0.0.1:8000")
	}
	return nil
}

func mustURL(s string) *url.URL {
	parsed, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return parsed
}
