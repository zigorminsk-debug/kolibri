package ui

import (
	"image/color"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// tone is the meaning a colour carries.
type tone int

const (
	toneNeutral tone = iota
	toneGood
	toneBusy
	toneBad
)

func (t tone) color() color.NRGBA {
	switch t {
	case toneGood:
		return colEmerald
	case toneBusy:
		return colAmber
	case toneBad:
		return colRuby
	}
	return colFaint
}

func text(s string, size float32, c color.Color, style fyne.TextStyle) *canvas.Text {
	t := canvas.NewText(s, c)
	t.TextSize = size
	t.TextStyle = style
	return t
}

// panel puts content on a raised, outlined surface.
func panel(content fyne.CanvasObject) *fyne.Container {
	bg := canvas.NewRectangle(colSurface)
	bg.CornerRadius = 12
	bg.StrokeColor = colLine
	bg.StrokeWidth = 1
	return container.NewStack(bg, container.NewPadded(container.NewPadded(content)))
}

// fixed gives an object an exact size inside flexible layouts.
func fixed(w, h float32, obj fyne.CanvasObject) *fyne.Container {
	return container.NewGridWrap(fyne.NewSize(w, h), obj)
}

func gap(w, h float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(w, h))
	return r
}

// fieldLabel is the small caption above a form control.
func fieldLabel(s string) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(6, 0, 2, 0), text(s, 12.5, colMuted, fyne.TextStyle{}))
}

// ---- status chip --------------------------------------------------------

// chip is a pill that shows a connection state: a coloured dot whose
// meaning is always repeated in the words next to it.
type chip struct {
	widget.BaseWidget
	bg    *canvas.Rectangle
	dot   *canvas.Circle
	label *canvas.Text
	body  *fyne.Container
	onTap func()
	// onResize is called when the label changes, so that the surrounding
	// layout can give the chip its new width.
	onResize func()
}

func newChip(label string, t tone, onTap func()) *chip {
	c := &chip{onTap: onTap}
	c.bg = canvas.NewRectangle(colSurface)
	c.bg.CornerRadius = 15
	c.bg.StrokeColor = colLine
	c.bg.StrokeWidth = 1
	c.dot = canvas.NewCircle(t.color())
	c.label = text(label, 13, colText, fyne.TextStyle{})
	row := container.NewHBox(gap(4, 0), container.NewCenter(fixed(8, 8, c.dot)), gap(1, 0), c.label, gap(6, 0))
	c.body = container.NewStack(c.bg, container.New(layout.NewCustomPaddedLayout(6, 6, 6, 6), row))
	c.ExtendBaseWidget(c)
	return c
}

func (c *chip) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(c.body) }

func (c *chip) Set(label string, t tone) {
	c.label.Text = label
	c.dot.FillColor = t.color()
	c.body.Refresh()
	c.Refresh()
	if c.onResize != nil {
		c.onResize()
	}
}

func (c *chip) Tapped(*fyne.PointEvent) {
	if c.onTap != nil {
		c.onTap()
	}
}

func (c *chip) Cursor() desktop.Cursor { return desktop.PointerCursor }

func (c *chip) MouseIn(*desktop.MouseEvent) {
	c.bg.FillColor = colRaised
	c.bg.Refresh()
}
func (c *chip) MouseMoved(*desktop.MouseEvent) {}
func (c *chip) MouseOut() {
	c.bg.FillColor = colSurface
	c.bg.Refresh()
}

// ---- stage tracker ------------------------------------------------------

type stageState int

const (
	stagePending stageState = iota
	stageActive
	stageDone
	stageFailed
)

// stageBar is the flight path of a task: five stations joined by a line.
// The active one pulses, because a local model can sit on one station for
// minutes and the window must not look frozen.
type stageBar struct {
	widget.BaseWidget
	names   []string
	states  []stageState
	circles []*canvas.Circle
	marks   []*canvas.Text
	labels  []*canvas.Text
	links   []*canvas.Rectangle
	pulse   *canvas.Circle
	anim    *fyne.Animation
	active  int
}

const (
	stageRadius = 14
	stageLabelH = 18
)

func newStageBar(names ...string) *stageBar {
	b := &stageBar{names: names, states: make([]stageState, len(names)), active: -1}
	for i := range names {
		c := canvas.NewCircle(colSurface)
		c.StrokeWidth = 1.5
		b.circles = append(b.circles, c)
		m := text("", 13, colMuted, fyne.TextStyle{Bold: true})
		m.Alignment = fyne.TextAlignCenter
		b.marks = append(b.marks, m)
		l := text(names[i], 13, colMuted, fyne.TextStyle{})
		l.Alignment = fyne.TextAlignCenter
		b.labels = append(b.labels, l)
		if i > 0 {
			b.links = append(b.links, canvas.NewRectangle(colLine))
		}
	}
	b.pulse = canvas.NewCircle(color.Transparent)
	b.pulse.StrokeWidth = 2
	b.pulse.Hide()
	b.ExtendBaseWidget(b)
	b.apply()
	return b
}

// Set changes the state of every station at once.
func (b *stageBar) Set(states []stageState) {
	copy(b.states, states)
	b.apply()
	b.Refresh()
}

func (b *stageBar) apply() {
	active := -1
	for i, st := range b.states {
		c, m, l := b.circles[i], b.marks[i], b.labels[i]
		m.Text = string(rune('1' + i))
		l.TextStyle = fyne.TextStyle{}
		switch st {
		case stagePending:
			c.FillColor, c.StrokeColor = colSurface, colLine
			m.Color, l.Color = colMuted, colMuted
		case stageActive:
			active = i
			c.FillColor, c.StrokeColor = colAmber, colAmber
			m.Color, l.Color = colOnAccent, colText
			l.TextStyle = fyne.TextStyle{Bold: true}
		case stageDone:
			c.FillColor, c.StrokeColor = withAlpha(colEmerald, 0x2E), colEmerald
			m.Text = "✓"
			m.Color, l.Color = colEmerald, colText
		case stageFailed:
			c.FillColor, c.StrokeColor = colRuby, colRuby
			m.Text = "×"
			m.Color, l.Color = colOnAccent, colRuby
			l.TextStyle = fyne.TextStyle{Bold: true}
		}
		if i > 0 {
			link := b.links[i-1]
			link.FillColor = colLine
			if b.states[i-1] == stageDone && st != stagePending {
				link.FillColor = withAlpha(colEmerald, 0xB0)
			}
		}
	}
	if active != b.active {
		b.active = active
		if b.anim != nil {
			b.anim.Stop()
			b.anim = nil
		}
		if active < 0 {
			b.pulse.Hide()
		} else {
			b.pulse.Show()
			b.anim = fyne.NewAnimation(1600*time.Millisecond, b.tick)
			b.anim.RepeatCount = fyne.AnimationRepeatForever
			b.anim.Curve = fyne.AnimationEaseOut
			b.anim.Start()
		}
	}
}

// tick grows and fades the ring around the active station.
func (b *stageBar) tick(p float32) {
	if b.active < 0 {
		return
	}
	center := b.center(b.active, b.Size())
	r := float32(stageRadius) + 1 + 9*p
	b.pulse.StrokeColor = withAlpha(colAmber, uint8(150*(1-p)))
	b.pulse.Resize(fyne.NewSize(2*r, 2*r))
	b.pulse.Move(fyne.NewPos(center.X-r, center.Y-r))
	b.pulse.Refresh()
}

func (b *stageBar) center(i int, size fyne.Size) fyne.Position {
	col := size.Width / float32(len(b.names))
	return fyne.NewPos(col*(float32(i)+0.5), 10+stageRadius)
}

func (b *stageBar) CreateRenderer() fyne.WidgetRenderer {
	objects := []fyne.CanvasObject{b.pulse}
	for _, l := range b.links {
		objects = append(objects, l)
	}
	for i := range b.names {
		objects = append(objects, b.circles[i], b.marks[i], b.labels[i])
	}
	return &stageRenderer{bar: b, objects: objects}
}

type stageRenderer struct {
	bar     *stageBar
	objects []fyne.CanvasObject
}

func (r *stageRenderer) Destroy() {
	if r.bar.anim != nil {
		r.bar.anim.Stop()
	}
}

func (r *stageRenderer) MinSize() fyne.Size {
	return fyne.NewSize(float32(len(r.bar.names))*96, 10+2*stageRadius+8+stageLabelH+4)
}

func (r *stageRenderer) Layout(size fyne.Size) {
	b := r.bar
	col := size.Width / float32(len(b.names))
	for i := range b.names {
		c := b.center(i, size)
		b.circles[i].Resize(fyne.NewSize(2*stageRadius, 2*stageRadius))
		b.circles[i].Move(fyne.NewPos(c.X-stageRadius, c.Y-stageRadius))
		ms := b.marks[i].MinSize()
		b.marks[i].Resize(fyne.NewSize(2*stageRadius, ms.Height))
		b.marks[i].Move(fyne.NewPos(c.X-stageRadius, c.Y-ms.Height/2))
		b.labels[i].Resize(fyne.NewSize(col, stageLabelH))
		b.labels[i].Move(fyne.NewPos(col*float32(i), c.Y+stageRadius+8))
		if i > 0 {
			prev := b.center(i-1, size)
			link := b.links[i-1]
			link.Resize(fyne.NewSize(c.X-prev.X-2*stageRadius-12, 2))
			link.Move(fyne.NewPos(prev.X+stageRadius+6, c.Y-1))
		}
	}
	b.tick(0)
}

func (r *stageRenderer) Objects() []fyne.CanvasObject { return r.objects }

func (r *stageRenderer) Refresh() {
	r.Layout(r.bar.Size())
	for _, o := range r.objects {
		o.Refresh()
	}
}

// ---- small helpers ------------------------------------------------------

// iconButton is a quiet, icon-only button with a tooltip-like label for
// assistive technology.
func iconButton(icon fyne.Resource, onTap func()) *widget.Button {
	b := widget.NewButtonWithIcon("", icon, onTap)
	b.Importance = widget.LowImportance
	return b
}

func quietButton(label string, icon fyne.Resource, onTap func()) *widget.Button {
	b := widget.NewButtonWithIcon(label, icon, onTap)
	b.Importance = widget.LowImportance
	return b
}

func hint(s string) *widget.Label {
	l := widget.NewLabel(s)
	l.Importance = widget.LowImportance
	l.Wrapping = fyne.TextWrapWord
	l.SizeName = theme.SizeNameCaptionText
	return l
}

// flowLayout puts objects in a row and wraps them onto further rows when the
// row is too narrow, so that a long row of choices does not dictate the
// minimum width of the window.
//
// A Fyne layout learns its width only in Layout, so the height it asks for
// in MinSize is the one of the last layout. When the number of rows changes,
// stale is set: the owner must refresh the parent container once more.
type flowLayout struct {
	width float32
	rows  int
	stale bool
}

func (l *flowLayout) arrange(objects []fyne.CanvasObject, width float32, place bool) (rows int, height float32) {
	pad := theme.Padding()
	var x, y, rowH float32
	for _, o := range objects {
		if !o.Visible() {
			continue
		}
		min := o.MinSize()
		if x > 0 && x+min.Width > width {
			x, y = 0, y+rowH+pad
			rowH = 0
			rows++
		}
		if rows == 0 {
			rows = 1
		}
		if place {
			o.Move(fyne.NewPos(x, y))
			o.Resize(min)
		}
		x += min.Width + pad
		rowH = fyne.Max(rowH, min.Height)
	}
	return rows, y + rowH
}

func (l *flowLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	rows, _ := l.arrange(objects, size.Width, true)
	assumed := l.rows
	if l.width <= 0 {
		assumed = 1 // what MinSize promised before the first layout
	}
	if rows != assumed {
		l.stale = true
	}
	l.rows, l.width = rows, size.Width
}

func (l *flowLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var widest float32
	for _, o := range objects {
		if o.Visible() {
			widest = fyne.Max(widest, o.MinSize().Width)
		}
	}
	width := l.width
	if width <= 0 {
		width = 1 << 20 // not laid out yet: assume there is room for one row
	}
	_, height := l.arrange(objects, width, false)
	return fyne.NewSize(widest, height)
}
