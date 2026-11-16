package ui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/screenshot"
)

func (u *UI) pickScreenshot() {
	if u.running {
		return
	}
	if len(u.shots) >= screenshot.MaxAttached {
		u.showProblem(fmt.Sprintf("К одной задаче можно приложить не более %d скриншотов.", screenshot.MaxAttached))
		return
	}
	fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil {
			u.do(func() { u.showProblem(err.Error()) })
			return
		}
		if rc == nil {
			return
		}
		name := rc.URI().Name()
		u.spawn(func() {
			defer rc.Close()
			raw, rerr := io.ReadAll(io.LimitReader(rc, screenshot.MaxInputBytes+1))
			if rerr != nil {
				u.do(func() { u.showProblem("Не удалось прочитать файл: " + rerr.Error()) })
				return
			}
			if len(raw) > screenshot.MaxInputBytes {
				u.do(func() {
					u.showProblem(fmt.Sprintf("Файл %s превышает %d МБ.", name, screenshot.MaxInputBytes>>20))
				})
				return
			}
			im, derr := screenshot.Decode(name, raw)
			u.do(func() {
				if derr != nil {
					u.showProblem(derr.Error())
					return
				}
				u.addScreenshot(im)
			})
		})
	}, u.win)
	fd.SetFilter(storage.NewExtensionFileFilter(screenshot.Extensions))
	fd.SetConfirmText("Прикрепить")
	fd.SetDismissText("Отмена")
	fd.Resize(fyne.NewSize(760, 520))
	fd.Show()
}

func (u *UI) pasteScreenshot() {
	if u.running {
		return
	}
	if len(u.shots) >= screenshot.MaxAttached {
		u.showProblem(fmt.Sprintf("К одной задаче можно приложить не более %d скриншотов.", screenshot.MaxAttached))
		return
	}
	var hint string
	if cb := u.app.Clipboard(); cb != nil {
		hint = cb.Content()
	}
	u.shotPaste.Disable()
	readClip := u.readClipboard
	if readClip == nil {
		readClip = screenshot.FromClipboard
	}
	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		im, err := readClip(ctx, hint)
		u.do(func() {
			if !u.running {
				u.shotPaste.Enable()
			}
			if err != nil {
				u.showProblem(err.Error())
				return
			}
			u.addScreenshot(im)
		})
	})
}

// onFilesDropped attaches screenshots dragged onto the Kolibri Forge window.
func (u *UI) onFilesDropped(_ fyne.Position, uris []fyne.URI) {
	if u.running || len(uris) == 0 {
		return
	}
	paths := make([]string, 0, len(uris))
	for _, uri := range uris {
		if uri == nil || uri.Scheme() != "file" {
			continue
		}
		p := uri.Path()
		if screenshot.IsImagePath(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		u.showProblem("Перетащите файл изображения (PNG, JPEG, BMP или GIF).")
		return
	}
	u.spawn(func() {
		for _, p := range paths {
			im, err := screenshot.LoadFile(p)
			u.do(func() {
				if err != nil {
					u.showProblem(err.Error())
					return
				}
				u.addScreenshot(im)
			})
		}
	})
}

// attachScreenshotFile loads path synchronously and adds it to the task form.
func (u *UI) attachScreenshotFile(path string) error {
	im, err := screenshot.LoadFile(path)
	if err != nil {
		return err
	}
	u.addScreenshot(im)
	return nil
}

func (u *UI) addScreenshot(im screenshot.Image) {
	if len(u.shots) >= screenshot.MaxAttached {
		u.showProblem(fmt.Sprintf("Максимум %d скриншотов на одну задачу.", screenshot.MaxAttached))
		return
	}
	u.clearProblem()
	u.shots = append(u.shots, im)
	u.refreshShotBar()
}

func (u *UI) removeScreenshot(idx int) {
	if idx < 0 || idx >= len(u.shots) {
		return
	}
	u.shots = append(u.shots[:idx], u.shots[idx+1:]...)
	u.refreshShotBar()
}

func (u *UI) clearScreenshots() {
	if len(u.shots) == 0 {
		return
	}
	u.shots = nil
	u.refreshShotBar()
}

func (u *UI) refreshShotBar() {
	if u.shotBar == nil {
		return
	}
	u.shotBar.Objects = nil
	if len(u.shots) == 0 {
		u.shotBar.Hide()
		if u.formTop != nil {
			u.formTop.Refresh()
		}
		if u.formBody != nil {
			u.formBody.Refresh()
		}
		return
	}
	for i, im := range u.shots {
		idx := i
		shot := im
		title := clipShotName(filepath.Base(shot.Name), 22)
		openBtn := widget.NewButtonWithIcon(title, theme.FileImageIcon(), func() {
			u.previewScreenshot(shot)
		})
		openBtn.Importance = widget.LowImportance
		delBtn := widget.NewButtonWithIcon("", theme.ContentClearIcon(), func() {
			if !u.running {
				u.removeScreenshot(idx)
			}
		})
		delBtn.Importance = widget.LowImportance
		if u.running {
			delBtn.Disable()
		}
		bg := canvas.NewRectangle(colRaised)
		bg.CornerRadius = 6
		bg.StrokeColor = colLine
		bg.StrokeWidth = 1
		pill := container.NewStack(bg, container.NewThemeOverride(container.NewHBox(openBtn, delBtn), denseTheme{}))
		u.shotBar.Add(pill)
	}
	u.shotBar.Show()
	if u.formTop != nil {
		u.formTop.Refresh()
	}
	if u.formBody != nil {
		u.formBody.Refresh()
	}
}

func (u *UI) previewScreenshot(im screenshot.Image) {
	img := canvas.NewImageFromReader(bytes.NewReader(im.Data), im.Name)
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(560, 340))

	meta := fmt.Sprintf("%s · %d×%d · %d КБ", im.Name, im.Width, im.Height, max(1, len(im.Data)>>10))
	if im.OrigWidth > 0 && (im.OrigWidth != im.Width || im.OrigHeight != im.Height) {
		meta = fmt.Sprintf("%s · исходный %d×%d → %d×%d · %d КБ",
			im.Name, im.OrigWidth, im.OrigHeight, im.Width, im.Height, max(1, len(im.Data)>>10))
	}
	cap := hint(meta + ". Этот снимок будет отправлен Колибри вместе с описанием задачи.")
	body := container.NewBorder(nil, cap, nil, nil, img)
	dlg := dialog.NewCustom(im.Name, "Закрыть", body, u.win)
	dlg.Resize(fyne.NewSize(720, 480))
	dlg.Show()
}

func clipShotName(name string, maxRunes int) string {
	r := []rune(name)
	if len(r) <= maxRunes {
		return name
	}
	ext := []rune(filepath.Ext(name))
	if len(ext) > 0 && len(ext) < maxRunes-6 {
		keep := maxRunes - len(ext) - 1
		return string(r[:keep]) + "…" + string(ext)
	}
	return string(r[:maxRunes-1]) + "…"
}
