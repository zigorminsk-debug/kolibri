package ui

import (
	_ "embed"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// The palette is taken from the bird: emerald plumage on a night-garden
// background, with the ruby throat reserved for stop and failure.
var (
	colInk      = color.NRGBA{0x0F, 0x14, 0x12, 0xFF} // window background
	colSurface  = color.NRGBA{0x16, 0x1D, 0x1A, 0xFF} // panels
	colRaised   = color.NRGBA{0x1E, 0x28, 0x24, 0xFF} // inputs, hovered rows
	colLine     = color.NRGBA{0x2B, 0x38, 0x33, 0xFF} // borders, separators
	colText     = color.NRGBA{0xE4, 0xEE, 0xE9, 0xFF}
	colMuted    = color.NRGBA{0x9B, 0xAF, 0xA7, 0xFF}
	colFaint    = color.NRGBA{0x7C, 0x90, 0x88, 0xFF}
	colEmerald  = color.NRGBA{0x34, 0xD8, 0xA2, 0xFF}
	colOnAccent = color.NRGBA{0x05, 0x1F, 0x16, 0xFF}
	colRuby     = color.NRGBA{0xF0, 0x52, 0x7A, 0xFF}
	colAmber    = color.NRGBA{0xF2, 0xB9, 0x50, 0xFF}
)

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

var (
	//go:embed assets/fonts/Onest-Regular.ttf
	fontRegular []byte
	//go:embed assets/fonts/Onest-SemiBold.ttf
	fontBold []byte
	//go:embed assets/fonts/JetBrainsMono-Regular.ttf
	fontMono []byte
	//go:embed assets/fonts/JetBrainsMono-Bold.ttf
	fontMonoBold []byte
	//go:embed assets/icon.png
	iconPNG []byte
	//go:embed assets/mark.png
	markPNG []byte

	resRegular  = fyne.NewStaticResource("Onest-Regular.ttf", fontRegular)
	resBold     = fyne.NewStaticResource("Onest-SemiBold.ttf", fontBold)
	resMono     = fyne.NewStaticResource("JetBrainsMono-Regular.ttf", fontMono)
	resMonoBold = fyne.NewStaticResource("JetBrainsMono-Bold.ttf", fontMonoBold)

	// Icon is the application icon.
	Icon = fyne.NewStaticResource("icon.png", iconPNG)
	mark = fyne.NewStaticResource("mark.png", markPNG)
)

// Theme is the application's look. It is dark on purpose and ignores the
// system light/dark preference: the window is a console to watch, and one
// carefully tuned palette beats two approximate ones.
type Theme struct{}

var _ fyne.Theme = Theme{}

func (Theme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return colInk
	case theme.ColorNameForeground:
		return colText
	case theme.ColorNamePlaceHolder:
		return colFaint
	case theme.ColorNameDisabled:
		return colFaint
	case theme.ColorNameButton:
		return colRaised
	case theme.ColorNameDisabledButton:
		return colSurface
	case theme.ColorNameInputBackground:
		return colRaised
	case theme.ColorNameInputBorder:
		return colLine
	case theme.ColorNameSeparator:
		return colLine
	case theme.ColorNamePrimary, theme.ColorNameHyperlink, theme.ColorNameSuccess:
		return colEmerald
	case theme.ColorNameFocus:
		return withAlpha(colEmerald, 0xB0)
	case theme.ColorNameSelection:
		return withAlpha(colEmerald, 0x40)
	case theme.ColorNameHover:
		return withAlpha(colText, 0x12)
	case theme.ColorNamePressed:
		return withAlpha(colText, 0x24)
	case theme.ColorNameError:
		return colRuby
	case theme.ColorNameWarning:
		return colAmber
	case theme.ColorNameForegroundOnPrimary, theme.ColorNameForegroundOnSuccess,
		theme.ColorNameForegroundOnWarning, theme.ColorNameForegroundOnError:
		return colOnAccent
	case theme.ColorNameHeaderBackground:
		return colSurface
	case theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return colSurface
	case theme.ColorNameScrollBar:
		return withAlpha(colMuted, 0x70)
	case theme.ColorNameScrollBarBackground:
		return color.Transparent
	case theme.ColorNameShadow:
		return color.NRGBA{0, 0, 0, 0x80}
	}
	return theme.DefaultTheme().Color(name, theme.VariantDark)
}

func (Theme) Font(style fyne.TextStyle) fyne.Resource {
	switch {
	case style.Monospace && style.Bold:
		return resMonoBold
	case style.Monospace:
		return resMono
	case style.Bold:
		return resBold
	case style.Symbol:
		return theme.DefaultTheme().Font(style)
	}
	return resRegular
}

func (Theme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (Theme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 14
	case theme.SizeNameCaptionText:
		return 12
	case theme.SizeNameSubHeadingText:
		return 17
	case theme.SizeNameHeadingText:
		return 22
	case theme.SizeNamePadding:
		return 5
	case theme.SizeNameInnerPadding:
		return 9
	case theme.SizeNameInputRadius, theme.SizeNameButtonRadius, theme.SizeNameSelectionRadius:
		return 7
	case theme.SizeNameCardRadius, theme.SizeNamePopupRadius, theme.SizeNameMenuRadius:
		return 10
	case theme.SizeNameDialogRadius:
		return 14
	case theme.SizeNameScrollBar:
		return 10
	case theme.SizeNameScrollBarSmall:
		return 4
	case theme.SizeNameScrollBarRadius:
		return 5
	}
	return theme.DefaultTheme().Size(name)
}

// denseTheme tightens paddings for lists of many short rows (the journal,
// the job list), where the default spacing wastes half the height.
type denseTheme struct{ Theme }

func (d denseTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameInnerPadding:
		return 4
	case theme.SizeNamePadding:
		return 3
	}
	return d.Theme.Size(name)
}
