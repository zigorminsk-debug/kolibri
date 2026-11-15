// Kolibri Forge gives tasks to a local colibri model, commits the code it
// writes to GitHub and lets GitHub Actions turn it into a finished program.
package main

import (
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"

	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/runlog"
	"github.com/zigorminsk-debug/kolibri/internal/ui"
)

func main() {
	a := app.NewWithID("io.github.zigorminsk-debug.kolibri-forge")
	a.Settings().SetTheme(ui.Theme{})
	a.SetIcon(ui.Icon)

	cfg, loadErr := config.Load()

	w := a.NewWindow(ui.AppName)
	w.SetIcon(ui.Icon)
	// Closing the main window closes the help window too and ends the program.
	w.SetMaster()
	w.Resize(fyne.NewSize(1240, 800))
	w.CenterOnScreen()

	view := ui.New(a, w, cfg)
	// The log lives next to the settings: it survives a change of the
	// output folder and is not mixed with downloaded programs.
	if path, err := config.Path(); err == nil {
		view.Log = runlog.New(filepath.Join(filepath.Dir(path), "kolibri-forge.log"))
	}
	w.SetOnClosed(view.Stop)
	view.Start()
	if loadErr != nil {
		dialog.ShowError(loadErr, w)
	}
	w.ShowAndRun()
}
