package ui

import (
	"net/http"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/update"
)

// allText collects every piece of text drawn inside an object.
func allText(root fyne.CanvasObject) string {
	var b strings.Builder
	var rec func(o fyne.CanvasObject)
	rec = func(o fyne.CanvasObject) {
		if o == nil || !o.Visible() {
			return
		}
		switch v := o.(type) {
		case *canvas.Text:
			b.WriteString(v.Text + "\n")
		case *fyne.Container:
			for _, child := range v.Objects {
				rec(child)
			}
		case fyne.Widget:
			for _, child := range test.WidgetRenderer(v).Objects() {
				rec(child)
			}
		}
	}
	rec(root)
	return b.String()
}

func release(tag string, pre bool, notes string, files ...string) map[string]any {
	assets := []map[string]any{}
	for _, f := range files {
		assets = append(assets, map[string]any{"name": f, "size": 1 << 20, "browser_download_url": "https://github.com/zigorminsk-debug/kolibri/releases/download/" + tag + "/" + f})
	}
	return map[string]any{
		"tag_name": tag, "name": tag, "prerelease": pre, "body": notes, "published_at": "2026-11-10T09:00:00Z",
		"html_url": "https://github.com/zigorminsk-debug/kolibri/releases/tag/" + tag, "assets": assets,
	}
}

var allFiles = []string{"KolibriForge-Setup.exe", "KolibriForge-windows-amd64.zip", "KolibriForge-linux-amd64.tar.gz", "KolibriForge-macos-arm64.zip"}

func TestUpdateCheckAtStart(t *testing.T) {
	h := newFakeHub(t)
	h.releases = []map[string]any{
		release("build-61", true, "Промежуточная сборка", allFiles...),
		release("v1.1.0", false, "## Что нового\n\n- Сборка под Android\n- Справка", allFiles...),
		release("v1.0.0", false, "Первая версия", allFiles...),
	}
	u := hubUI(t, h, "good")
	u.version = &update.Current{Version: "1.0.0", Build: 40}
	u.goosOverride = "windows"
	if u.updateBar.Visible() {
		t.Fatal("the update strip is shown before any check")
	}
	// (Start itself is not used here: its ticker redraws from a goroutine of
	// its own, which the test driver does not put on the UI goroutine.)
	u.checkUpdatesAtStart()
	u.bg.Wait()

	if !u.updateBar.Visible() || u.updateFound == nil || u.updateFound.Release.TagName != "v1.1.0" {
		t.Fatalf("update strip: visible=%v found=%+v", u.updateBar.Visible(), u.updateFound)
	}
	if got := allText(u.updateBar); !strings.Contains(got, "Доступно обновление "+AppName+": 1.1.0") || !strings.Contains(got, "Скачать") {
		t.Errorf("update strip says:\n%s", got)
	}
	// The check is anonymous: the user's token is not sent for it.
	if len(h.releaseAuth) != 1 || h.releaseAuth[0] != "" {
		t.Errorf("Authorization headers of the update check: %q", h.releaseAuth)
	}
	snap(t, u.win, "50-update-strip")

	// "Download" leads to the installer on Windows, to the archive elsewhere.
	if link, label := updateTarget(*u.updateFound, "windows"); !strings.HasSuffix(link, "/v1.1.0/KolibriForge-Setup.exe") || label != "Скачать установщик" {
		t.Errorf("windows: %s %q", link, label)
	}
	if link, label := updateTarget(*u.updateFound, "linux"); !strings.HasSuffix(link, "/v1.1.0/KolibriForge-linux-amd64.tar.gz") || label != "Скачать архив" {
		t.Errorf("linux: %s %q", link, label)
	}
	if link, label := updateTarget(*u.updateFound, "freebsd"); !strings.HasSuffix(link, "/releases/tag/v1.1.0") || label != "Открыть страницу релиза" {
		t.Errorf("freebsd: %s %q", link, label)
	}

	// Details, and "skip this version": remembered, the strip goes away.
	var saved config.Config
	u.Save = func(c config.Config) error { saved = c; return nil }
	u.showUpdate(*u.updateFound)
	details := allText(u.win.Canvas().Overlays().Top())
	for _, want := range []string{AppName + " — 1.1.0", "Опубликовано 10.11.2026", "1.0.0 · сборка 40", "Сборка под Android", "Пропустить эту версию", "Скачать установщик"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
	}
	snap(t, u.win, "51-update-details")
	tapButton(t, u.win.Canvas().Overlays().Top(), "Пропустить эту версию")
	if saved.UpdateSkipped != "v1.1.0" || u.cfg.UpdateSkipped != "v1.1.0" || u.updateBar.Visible() {
		t.Errorf("skip: saved=%q cfg=%q strip visible=%v", saved.UpdateSkipped, u.cfg.UpdateSkipped, u.updateBar.Visible())
	}

	// The next start does not repeat the declined offer...
	u.checkUpdates(nil)
	u.bg.Wait()
	if u.updateBar.Visible() {
		t.Error("a skipped version is offered again")
	}
	// ...but a newer one is offered.
	h.mu.Lock()
	h.releases = append([]map[string]any{release("v1.2.0", false, "", allFiles...)}, h.releases...)
	h.mu.Unlock()
	u.checkUpdates(nil)
	u.bg.Wait()
	if !u.updateBar.Visible() || u.updateFound.Release.TagName != "v1.2.0" {
		t.Errorf("newer version: visible=%v found=%+v", u.updateBar.Visible(), u.updateFound)
	}
}

func TestUpdateCheckStaysQuiet(t *testing.T) {
	cases := []struct {
		name     string
		cur      update.Current
		setup    func(c *config.Config, h *fakeHub)
		requests int
	}{
		{"up to date", update.Current{Version: "1.1.0", Build: 61}, nil, 1},
		{"switched off", update.Current{Version: "1.0.0", Build: 1}, func(c *config.Config, _ *fakeHub) { c.CheckUpdates = false }, 0},
		{"built from source", update.Current{}, nil, 0},
		{"no releases yet", update.Current{Version: "1.0.0", Build: 1}, func(_ *config.Config, h *fakeHub) { h.releases = nil }, 1},
		{"GitHub is down", update.Current{Version: "1.0.0", Build: 1}, func(_ *config.Config, h *fakeHub) { h.releasesStatus = http.StatusBadGateway }, 1},
		{"repository hidden", update.Current{Version: "1.0.0", Build: 1}, func(_ *config.Config, h *fakeHub) { h.releasesStatus = http.StatusNotFound }, 1},
		{"builds not wanted", update.Current{Version: "1.1.0", Build: 50}, func(c *config.Config, _ *fakeHub) { c.UpdateBuilds = false }, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeHub(t)
			h.releases = []map[string]any{release("build-61", true, ""), release("v1.1.0", false, "")}
			u := hubUI(t, h, "good")
			if c.setup != nil {
				c.setup(&u.cfg, h)
			}
			cur := c.cur
			u.version = &cur
			u.checkUpdatesAtStart()
			u.bg.Wait()
			if u.updateBar.Visible() {
				t.Errorf("update strip shown: %s", allText(u.updateBar))
			}
			if len(h.releaseAuth) != c.requests {
				t.Errorf("%d requests for releases, want %d", len(h.releaseAuth), c.requests)
			}
			if u.problem.Visible() {
				t.Errorf("the check bothered the user: %q", u.problem.Text)
			}
		})
	}
}

func TestUpdateCheckByHand(t *testing.T) {
	h := newFakeHub(t)
	h.releases = []map[string]any{release("build-61", true, ""), release("v1.1.0", false, "")}
	u := hubUI(t, h, "")
	st := newStatus()
	button := widget.NewButton("Проверить обновления", nil)
	check := func(cur update.Current) string {
		t.Helper()
		u.version = &cur
		u.hideUpdateBar()
		u.checkUpdatesInto(button, st)
		u.bg.Wait()
		if button.Disabled() {
			t.Error("the button stayed disabled")
		}
		return st.label.Text
	}

	if got := check(update.Current{Version: "1.0.0", Build: 5}); got != "Доступно обновление: 1.1.0." || !u.updateBar.Visible() {
		t.Errorf("older: %q, strip=%v", got, u.updateBar.Visible())
	}
	if got := check(update.Current{Version: "1.1.0", Build: 5}); got != "Доступно обновление: сборка 61." {
		t.Errorf("older build: %q", got)
	}
	if got := check(update.Current{Version: "1.1.0", Build: 61}); got != "У вас последняя версия." || u.updateBar.Visible() {
		t.Errorf("up to date: %q, strip=%v", got, u.updateBar.Visible())
	}
	// A skipped version is still reported when asked by hand.
	u.cfg.UpdateSkipped = "v1.1.0"
	if got := check(update.Current{Version: "1.0.0", Build: 5}); got != "Доступно обновление: 1.1.0." || !u.updateBar.Visible() {
		t.Errorf("skipped, by hand: %q, strip=%v", got, u.updateBar.Visible())
	}
	// Built from source: nothing to compare with, so just name the latest.
	if got := check(update.Current{}); !strings.Contains(got, "Последний релиз на GitHub — 1.1.0") || !strings.Contains(got, "из исходников") || u.updateBar.Visible() {
		t.Errorf("from source: %q, strip=%v", got, u.updateBar.Visible())
	}
	h.mu.Lock()
	h.releasesStatus = http.StatusBadGateway
	h.mu.Unlock()
	if got := check(update.Current{Version: "1.0.0", Build: 5}); !strings.HasPrefix(got, "Не удалось проверить: ") {
		t.Errorf("failure: %q", got)
	}
}

func TestHelpSections(t *testing.T) {
	sections := helpSections(helpText)
	if len(sections) < 12 {
		t.Fatalf("%d help sections", len(sections))
	}
	if sections[0].Title != "Быстрый старт" || sections[len(sections)-1].Title != aboutTitle || sections[len(sections)-1].Body != "" {
		t.Errorf("first %q, last %q", sections[0].Title, sections[len(sections)-1].Title)
	}
	seen := map[string]bool{}
	for _, s := range sections[:len(sections)-1] {
		if seen[s.Title] {
			t.Errorf("section %q twice", s.Title)
		}
		seen[s.Title] = true
		if len([]rune(s.Body)) < 200 {
			t.Errorf("section %q is nearly empty", s.Title)
		}
		// The help is drawn by Fyne's Markdown: no tables, no HTML.
		for _, line := range strings.Split(s.Body, "\n") {
			if strings.HasPrefix(line, "|") || strings.Contains(line, "<br") || strings.HasPrefix(line, "# ") {
				t.Errorf("section %q: line %q will not render", s.Title, line)
			}
		}
	}
	// Sections the help refers to by name must exist.
	for _, title := range []string{"Типы проектов", "Системы и Android", "Поиск в интернете и Lightpanda", "Обновление программы", "Готовые программы и релизы"} {
		if !seen[title] {
			t.Errorf("no section %q", title)
		}
		if !strings.Contains(helpText, "«"+title+"»") {
			t.Errorf("section %q is never referred to", title)
		}
	}
	// What the help says about the form must match the form.
	u, _ := newTestUI(t, config.Default(), 1240, 800)
	for _, k := range u.kindInfos {
		if !strings.Contains(helpText, k.Title) {
			t.Errorf("the help does not describe the project kind %q", k.Title)
		}
	}
	for _, label := range []string{u.useWeb.Text, u.private.Text, u.release.Text, "Чинить упавшую сборку", "Проверить Lightpanda", "Установить Lightpanda", "Проверять обновления при запуске", "Предлагать промежуточные сборки", "Проверить обновления"} {
		if !strings.Contains(helpText, label) {
			t.Errorf("the help does not mention %q", label)
		}
	}
}

func TestHelpWindow(t *testing.T) {
	h := newFakeHub(t)
	u := hubUI(t, h, "")
	u.version = &update.Current{Version: "1.0.0", Build: 12}
	u.showHelp("")
	win, v := u.helpWin, u.helpView
	if win == nil || v == nil {
		t.Fatal("no help window")
	}
	if got := allText(v.page); !strings.Contains(got, "Быстрый старт") || !strings.Contains(got, "Пять шагов") {
		t.Errorf("first page:\n%s", got)
	}
	snap(t, win, "60-help")

	// Asking again does not open a second window; it turns the page.
	u.showHelp("Системы и Android")
	if u.helpWin != win {
		t.Error("a second help window was opened")
	}
	if got := allText(v.page); !strings.Contains(got, "отладочным ключом") {
		t.Errorf("android page:\n%s", got)
	}
	snap(t, win, "61-help-android")

	u.showHelp(aboutTitle)
	about := allText(v.page)
	for _, want := range []string{AppName, "1.0.0 · сборка 12", DeveloperName, DeveloperEmail, DeveloperPhone, "github.com/zigorminsk-debug/kolibri", "Проверить обновления"} {
		if !strings.Contains(about, want) {
			t.Errorf("the about page lacks %q:\n%s", want, about)
		}
	}
	if DeveloperName != "Zakharevich Igor" || DeveloperEmail != "ziv@csl.by" || DeveloperPhone != "+375293371412" {
		t.Error("developer details changed")
	}
	snap(t, win, "62-help-about")

	// The button on the about page checks for updates.
	h.releases = []map[string]any{release("v1.1.0", false, "")}
	test.Tap(v.checkUpdate)
	u.bg.Wait()
	if v.updateStatus.label.Text != "Доступно обновление: 1.1.0." || !u.updateBar.Visible() {
		t.Errorf("about: %q, strip=%v", v.updateStatus.label.Text, u.updateBar.Visible())
	}
	snap(t, win, "63-help-about-update")

	// Every page can be drawn.
	for i := range v.sections {
		v.open(i)
		if allText(v.page) == "" {
			t.Errorf("section %q is blank", v.sections[i].Title)
		}
	}

	win.Close()
	if u.helpWin != nil || u.helpView != nil {
		t.Error("the closed help window is still remembered")
	}
	u.showHelp("")
	if u.helpWin == nil {
		t.Fatal("help does not open again")
	}
	u.Stop()
	if u.helpWin != nil {
		t.Error("the help window outlives the program")
	}
}

// tapButton presses the button with this label somewhere inside root.
func tapButton(t *testing.T, root fyne.CanvasObject, label string) {
	t.Helper()
	var found *widget.Button
	var rec func(o fyne.CanvasObject)
	rec = func(o fyne.CanvasObject) {
		if o == nil || found != nil {
			return
		}
		switch v := o.(type) {
		case *widget.Button:
			if v.Text == label {
				found = v
			}
		case *fyne.Container:
			for _, child := range v.Objects {
				rec(child)
			}
		case fyne.Widget:
			for _, child := range test.WidgetRenderer(v).Objects() {
				rec(child)
			}
		}
	}
	rec(root)
	if found == nil {
		t.Fatalf("no button %q", label)
	}
	test.Tap(found)
}
