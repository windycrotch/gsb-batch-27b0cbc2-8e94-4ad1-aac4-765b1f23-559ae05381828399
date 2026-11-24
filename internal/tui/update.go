package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/Megge06/TermiCam/internal/video"
)

// playbackTickMsg paces playback. It carries the session id so a tick belonging
// to a session that has since been closed is ignored instead of reading frames.
type playbackTickMsg struct{ sessionID uint64 }

// Define how the model updates
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		// Do not quit on "q" if an input field is active or the list is filtering
		isTyping := m.inputActive || (m.screen == screenSelect && m.deviceList.FilterState() == list.Filtering)

		if keyMsg.String() == "ctrl+c" || (keyMsg.String() == "q" && !isTyping) {
			m.shutdown()
			return m, tea.Quit
		}
	}

	// Handle window size changes
	switch msg := msg.(type) {
	// In case of playing back a recording
	case playbackTickMsg:
		if m.session == nil || m.session.id != msg.sessionID || m.session.isClosed() {
			// Stale tick from a session that has already ended.
			return m, nil
		}
		if !m.session.playback {
			return m, nil
		}
		return m, m.startRead()

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height

		// Reserve some height for the logo block
		h := msg.Height - 8
		if h < 0 {
			h = 0
		}
		m.deviceList.SetSize(msg.Width, h) //

	case devicesLoadedMsg:
		m.loading = false
		m.devices = []video.Device(msg)
		m.choices = make([]string, len(m.devices))
		for i, device := range m.devices {
			m.choices[i] = device.String()
		}
		m.updateListItems()
		return m, nil
	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil
	}

	switch m.screen {
	case screenSelect:
		return m.updateSelect(msg)
	case screenCamera:
		return m.updateCamera(msg)
	case screenSettings:
		return m.updateSettings(msg)
	}

	return m, nil
}

// shutdown releases every resource owned by the model. It is safe to call
// multiple times and from any screen.
func (m *Model) shutdown() {
	m.closeSession()
}

func (m Model) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.inputActive {
		var cmd tea.Cmd
		m.textInput, cmd = m.textInput.Update(msg)

		if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
			switch keyMsg.String() {
			case "enter":
				// Confirm the change
				switch m.cursor {
				case 2:
					val := m.textInput.Value()
					if val != "" {
						m.customPalette = val
						m.persistCurrentSettings()
					}
				case 4:
					val := strings.TrimSpace(m.textInput.Value())
					if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
						m.fps = parsed
						m.persistCurrentSettings()
					} else {
						// Fallback if the entered value is invalid
						m.textInput.SetValue(strconv.Itoa(m.fps))
					}
				case 5:
					val := strings.TrimSpace(m.textInput.Value())
					if parsed, err := strconv.ParseFloat(val, 64); err == nil && parsed > 0 {
						m.ratio = parsed
						m.persistCurrentSettings()
					} else {
						// Fallback if the entered value is invalid
						m.textInput.SetValue(fmt.Sprintf("%.2f", m.ratio))
					}
				}
				m.textInput.Blur()
				m.inputActive = false
			case "esc":
				// Revert changes and exit edit mode
				m.textInput.Blur()
				m.inputActive = false
			}
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	// Handle keyboard inputs
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			// There are 6 items (0: Color, 1: Palette Mode, 2: Palette, 3: Mirror, 4: FPS, 5: Aspect Ratio, 6: Proceed)
			if m.cursor < 6 {
				m.cursor++
			}
		case "space", "left", "right":
			switch m.cursor {
			case 0:
				m.color = !m.color
				m.persistCurrentSettings()
			case 1:
				if msg.String() == "left" {
					switch m.paletteMode {
					case "detailed":
						m.paletteMode = "default"
					case "custom":
						m.paletteMode = "detailed"
					default:
						m.paletteMode = "custom"
					}
				} else {
					switch m.paletteMode {
					case "default":
						m.paletteMode = "detailed"
					case "detailed":
						m.paletteMode = "custom"
					default:
						m.paletteMode = "default"
					}
				}
				m.persistCurrentSettings()
			case 2:
				if m.paletteMode == "custom" {
					m.inputActive = true
					m.textInput.CharLimit = 100
					m.textInput.SetWidth(40)
					m.textInput.SetValue(m.customPalette)
					m.textInput.Focus()
					return m, textinput.Blink
				}
			case 3:
				m.mirror = !m.mirror
				m.persistCurrentSettings()
			case 4:
				// Activate input on Space
				m.inputActive = true
				m.textInput.CharLimit = 3
				m.textInput.SetWidth(5)
				m.textInput.SetValue(strconv.Itoa(m.fps))
				m.textInput.Focus()
				return m, textinput.Blink
			case 5:
				m.inputActive = true
				m.textInput.CharLimit = 6
				m.textInput.SetWidth(6)
				m.textInput.SetValue(fmt.Sprintf("%.2f", m.ratio))
				m.textInput.Focus()
				return m, textinput.Blink
			case 6:
				m.persistCurrentSettings()
				m.screen = screenSelect
				m.cursor = 0
				return m, nil
			}

		case "enter":
			m.persistCurrentSettings()
			m.screen = screenSelect
			m.cursor = 0
			return m, nil

		}
	}
	return m, nil
}

// Update select screen
func (m Model) updateSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	wasFiltering := m.deviceList.FilterState() == list.Filtering

	var cmd tea.Cmd
	m.deviceList, cmd = m.deviceList.Update(msg)

	switch msg := msg.(type) {

	// Handle keyboard inputs
	case tea.KeyPressMsg:
		if m.loading || m.err != nil {
			return m, nil
		}

		// Prevent custom actions if the user is typing query filters inside the list searchbox
		if wasFiltering {
			return m, cmd
		}

		switch msg.String() {
		case "space":
			selectedItem := m.deviceList.SelectedItem()
			if selectedItem != nil {
				if devItem, ok := selectedItem.(deviceItem); ok {
					targetIdx := -1
					for i, d := range m.devices {
						if d.ID == devItem.device.ID {
							targetIdx = i
							break
						}
					}
					if targetIdx != -1 {
						if m.selected == targetIdx {
							m.selected = -1
						} else {
							m.selected = targetIdx
						}
						m.updateListItems()
					}
				}
			}
		case "esc", "backspace":
			m.screen = screenSettings
			m.cursor = 0
			return m, nil
		case "enter":
			idx := m.selected

			// If no device was explicitly checked with Space, fall back to the currently focused device item
			if idx == -1 {
				selectedItem := m.deviceList.SelectedItem()
				if selectedItem != nil {
					if devItem, ok := selectedItem.(deviceItem); ok {
						for i, d := range m.devices {
							if d.ID == devItem.device.ID {
								idx = i
								break
							}
						}
					}
				}
			}

			if idx >= 0 && idx < len(m.devices) {
				device := m.devices[idx]
				if device.ID == "" {
					return m, nil
				}

				// Always rebuild the session: the new one is bound to this
				// entry's frame size and the previous one is already closed.
				if err := m.enterLiveSession(device); err != nil {
					m.err = err
					return m, nil
				}

				m.screen = screenCamera
				m.cursor = 0
				return m, m.startRead()
			}
		}
	}
	return m, cmd
}

// Update camera screen
func (m Model) updateCamera(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case frameMsg:
		// Discard results from a session that is no longer the active one.
		if m.session == nil || m.session.id != msg.sessionID || m.session.isClosed() {
			return m, nil
		}
		m.session.reading = false

		m.session.swapBuffers()

		// Reads recorded frame if a recording is being written.
		if err := m.session.writeRecordedFrame(); err != nil {
			m.err = fmt.Errorf("failed to write recorded frame: %w", err)
			_ = m.session.stopRecording()
			return m, nil
		}

		if m.session.playback {
			return m, playbackTickCmd(m.session.id, m.fps)
		}

		// Fetch the next frame into the back buffer
		return m, m.startRead()

	case frameErrMsg:
		// A late error from a closed session must not tear down the new one.
		if m.session == nil || m.session.id != msg.sessionID {
			return m, nil
		}

		if msg.err != nil {
			m.err = msg.err
		}
		m.closeSession()
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "backspace":
			wasPlayback := m.isPlayback()
			m.closeSession()
			if wasPlayback {
				return m, tea.Quit
			}
			m.screen = screenSelect
		case "h":
			m.hideUI = !m.hideUI
		case "r":
			if m.session == nil || m.session.playback {
				return m, nil
			}
			if m.session.isRecording() {
				_ = m.session.stopRecording()
			} else {
				filename := recordingFilename()
				if err := m.startRecording(filename); err != nil {
					m.err = err
				}
			}
		}
	}
	return m, nil
}

// startRead dispatches one asynchronous frame read, unless the active session
// already has a read in flight. Keeping this guard on the Update goroutine
// ensures two reads never target the same buffer concurrently.
func (m Model) startRead() tea.Cmd {
	s := m.session
	if s == nil || s.isClosed() || s.reading {
		return nil
	}
	s.reading = true

	return readFrameCmd(s)
}

// readFrameCmd reads the next frame from s into s.back asynchronously. The
// resulting message carries the session id so a result that arrives after the
// session has been replaced is discarded instead of writing into the new
// session's buffers.
func readFrameCmd(s *cameraSession) tea.Cmd {
	if s == nil {
		return nil
	}
	id := s.id
	// Capture the destination now: a swap after this read completes must not
	// change the buffer this in-flight read is writing into.
	buf := s.back

	return func() tea.Msg {
		if err := s.source.ReadFrame(buf); err != nil {
			return frameErrMsg{sessionID: id, err: err}
		}

		return frameMsg{sessionID: id}
	}
}

// Paces playback to provided fps
func playbackTickCmd(sessionID uint64, fps int) tea.Cmd {
	if fps <= 0 {
		fps = 30
	}
	return tea.Tick(time.Second/time.Duration(fps), func(t time.Time) tea.Msg {
		return playbackTickMsg{sessionID: sessionID}
	})
}

// persistCurrentSettings saves the current settings to disk
func (m Model) persistCurrentSettings() {
	settings := PersistedSettings{
		Color:         m.color,
		PaletteMode:   m.paletteMode,
		CustomPalette: m.customPalette,
		Mirror:        m.mirror,
		FPS:           m.fps,
		Ratio:         m.ratio,
	}
	_ = SaveSettings(settings)
}
