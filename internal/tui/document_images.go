package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/document"
	"github.com/lancekrogers/peep/internal/render"
)

type documentImage struct {
	image      document.Image
	loading    bool
	err        string
	key, width int
}
type documentImageMsg struct {
	id, index, width int
	image            document.Image
	err              error
}
type documentSlot struct{ index, start, rows, columns int }

func (m *Model) cancelDocumentImages() {
	if m.docImageCancel != nil {
		m.docImageCancel()
		m.docImageCancel = nil
	}
	for _, entry := range m.docImages {
		if entry.loading {
			entry.loading = false
			entry.width = 0
		}
	}
}
func (m *Model) documentComposition() ([]document.Line, []documentSlot) {
	if m.doc == nil || m.docSource || len(m.docLayout.Assets) == 0 {
		return m.docLayout.Lines, nil
	}
	lines := make([]document.Line, 0, len(m.docLayout.Lines))
	var slots []documentSlot
	skip := make(map[int]bool)
	for index, a := range m.docLayout.Assets {
		entry := m.docImages[index]
		if a.Kind == "mermaid" && a.ReplaceFrom > 0 && m.graphics != nil && (entry == nil || entry.err == "") {
			for row := a.ReplaceFrom - 1; row < a.Line; row++ {
				skip[row] = true
			}
		}
	}
	asset := 0
	for i := 0; i <= len(m.docLayout.Lines); i++ {
		for asset < len(m.docLayout.Assets) && m.docLayout.Assets[asset].Line == i {
			a := m.docLayout.Assets[asset]
			if a.Kind == "image" || a.Kind == "mermaid" {
				if a.Kind == "mermaid" && m.graphics == nil {
					lines = append(lines, document.Line{Text: color("Mermaid source · diagram needs native graphics", muted), SourceLine: a.SourceLine})
					asset++
					continue
				}
				entry := m.docImages[asset]
				rows, cols := 6, min(120, m.documentWidth())
				if entry != nil && entry.err != "" {
					slots = append(slots, documentSlot{asset, len(lines), 1, cols})
					lines = append(lines, document.Line{Text: color(render.Safe(entry.err), muted), SourceLine: a.SourceLine})
				} else {
					if entry != nil && entry.image.Width > 0 {
						cols, rows = document.FitImage(entry.image.Width, entry.image.Height, m.documentWidth())
					}
					slots = append(slots, documentSlot{asset, len(lines), rows, cols})
					for y := 0; y < rows; y++ {
						text := ""
						if entry == nil || entry.loading || len(entry.image.PNG) == 0 {
							if y == 0 {
								text = color("Loading "+a.Kind+" · "+render.Safe(a.Alt), muted)
							}
						} else if m.graphics == nil && entry.width == m.documentWidth() && y < len(entry.image.Cells) {
							text = entry.image.Cells[y]
						}
						lines = append(lines, document.Line{Text: text, SourceLine: a.SourceLine})
					}
				}
			}
			asset++
		}
		if i < len(m.docLayout.Lines) && !skip[i] {
			lines = append(lines, m.docLayout.Lines[i])
		}
	}
	return lines, slots
}
func (m *Model) requestDocumentImages() tea.Cmd {
	if m.doc == nil || m.docSource || m.help || m.filePrompt != nil || m.editing || m.layout().preview == 0 {
		return nil
	}
	if m.docImages == nil {
		m.docImages = map[int]*documentImage{}
	}
	_, slots := m.documentComposition()
	wanted := map[int]bool{}
	for _, slot := range slots {
		if slot.start+slot.rows > m.docOffset && slot.start < m.docOffset+m.layout().body {
			wanted[slot.index] = true
			if len(wanted) == 8 {
				break
			}
		}
	}
	active := 0
	for i, e := range m.docImages {
		if !wanted[i] && !e.loading {
			e.image.PNG = nil
			e.image.Cells = nil
			e.width = 0
		}
		if e.loading {
			active++
		}
	}
	if m.docImageCancel == nil {
		m.docImageCtx, m.docImageCancel = context.WithCancel(m.ctx)
	}
	if m.docMermaid == nil {
		m.docMermaid = document.NewMermaid("")
	}
	renderer := m.docMermaid
	var cmds []tea.Cmd
	for _, slot := range slots {
		i := slot.index
		if !wanted[i] {
			continue
		}
		entry := m.docImages[i]
		if entry != nil && (entry.loading || entry.err != "" || entry.width == m.documentWidth()) {
			continue
		}
		if active >= 2 {
			break
		}
		active++
		width, id, source, path, ctx := m.documentWidth(), m.previewID, m.docLayout.Assets[i], m.doc.Path, m.docImageCtx
		var cached []byte
		if entry != nil {
			cached = entry.image.PNG
		}
		pending := &documentImage{loading: true, width: width}
		if entry != nil {
			pending.image.Width = entry.image.Width
			pending.image.Height = entry.image.Height
		}
		m.docImages[i] = pending
		cmds = append(cmds, func() tea.Msg {
			child, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var im document.Image
			var err error
			if len(cached) > 0 {
				im, err = document.ImageFromPNG(cached, width)
			} else if source.Kind == "mermaid" {
				im, err = renderer.Render(child, source.Source, width)
			} else {
				var resolved string
				resolved, err = document.ResolveImage(path, source.Ref)
				if err == nil {
					im, err = document.ReadImage(child, resolved, width)
				}
			}
			return documentImageMsg{id, i, width, im, err}
		})
	}
	return tea.Batch(cmds...)
}
func (m *Model) applyDocumentImage(v documentImageMsg) tea.Cmd {
	if v.id != m.previewID || m.doc == nil {
		return nil
	}
	_, before := m.documentComposition()
	m.docImageRevision++
	entry := &documentImage{image: v.image, width: v.width, key: m.docImageRevision}
	if v.err != nil {
		entry.err = v.err.Error()
		entry.image = document.Image{}
	}
	bytes := len(entry.image.PNG)
	for i, e := range m.docImages {
		if i != v.index {
			bytes += len(e.image.PNG)
		}
	}
	if bytes > 48<<20 {
		entry.err = "document image memory limit reached"
		entry.image = document.Image{}
	}
	m.docImages[v.index] = entry
	// An image result must not snap the viewport to the start of its source
	// block: many rendered rows (including a whole image) share that line.
	// Only compensate for geometry changed by this particular asset.
	_, after := m.documentComposition()
	for _, old := range before {
		if old.index != v.index {
			continue
		}
		for _, next := range after {
			if next.index != v.index {
				continue
			}
			if m.docOffset >= old.start+old.rows {
				m.docOffset += next.start + next.rows - old.start - old.rows
			} else if m.docOffset >= old.start {
				m.docOffset = next.start + min(m.docOffset-old.start, next.rows-1)
			}
			break
		}
		break
	}
	m.scrollDocument(0)
	return m.requestDocumentImages()
}
func (m *Model) restoreDocumentAnchor(anchor int) {
	lines, _ := m.documentComposition()
	m.docOffset = 0
	for i, line := range lines {
		if line.SourceLine > anchor {
			break
		}
		if i == 0 || lines[i-1].SourceLine != line.SourceLine {
			m.docOffset = i
		}
	}
	m.scrollDocument(0)
}
func (m *Model) documentGraphics(x, y int) []render.ImagePlacement {
	_, slots := m.documentComposition()
	var images []render.ImagePlacement
	for _, slot := range slots {
		entry := m.docImages[slot.index]
		if entry == nil || entry.loading || entry.err != "" || len(entry.image.PNG) == 0 {
			continue
		}
		top := max(slot.start, m.docOffset)
		bottom := min(slot.start+slot.rows, m.docOffset+m.layout().body)
		if top >= bottom {
			continue
		}
		im := entry.image
		cropY := (top - slot.start) * im.Height / slot.rows
		cropEnd := (bottom - slot.start) * im.Height / slot.rows
		images = append(images, render.ImagePlacement{PNG: im.PNG, Key: entry.key, Rect: render.Placement{X: x, Y: y + top - m.docOffset, Columns: slot.columns, Rows: bottom - top}, Y: cropY, Width: im.Width, Height: max(1, cropEnd-cropY)})
	}
	return images
}
func (m *Model) documentPosition() string {
	lines, _ := m.documentComposition()
	return fmt.Sprintf("%d/%d", min(m.docOffset+1, len(lines)), len(lines))
}
