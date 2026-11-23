package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/render"
	"github.com/lancekrogers/peep/internal/tui"
)

type rendererDetected bool

// Keep capability negotiation outside the media model. Tea has entered raw mode
// before Init runs; its normal reader handles keys throughout negotiation.
type probingModel struct {
	model         *tui.Model
	input         *render.ProbeInput
	output        *render.KittyWriter
	ctx           context.Context
	width, height int
}

func (m *probingModel) Init() tea.Cmd {
	return tea.Batch(m.model.Init(), func() tea.Msg { return rendererDetected(m.input.Detect(m.ctx, m.output)) })
}
func (m *probingModel) View() string { return m.model.View() }
func (m *probingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
	case rendererDetected:
		if !v {
			return m, nil
		}
		m.model.SetGraphics(m.output)
		if m.width == 0 || m.height == 0 {
			return m, nil
		}
		msg = tea.WindowSizeMsg{Width: m.width, Height: m.height}
	}
	_, cmd := m.model.Update(msg)
	return m, cmd
}
