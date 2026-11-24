package tui

import (
	"errors"
	"fmt"
	"os"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/Megge06/TermiCam/internal/ascii"
	"github.com/Megge06/TermiCam/internal/video"
)

type screenState int

const (
	screenSettings screenState = iota
	screenSelect
	screenCamera
)

// Injectable seams so the camera lifecycle can be exercised without a real
// camera or a local FFmpeg install.
var (
	openLiveSource  = defaultOpenLiveSource
	probeResolution = video.GetDeviceResolution
	probeFramerate  = video.GetDeviceFramerate
	cliArgs         = os.Args
)

func defaultOpenLiveSource(device video.Device, captureWidth, captureHeight, width, height, fps int, captureFPS float64) (video.FrameSource, error) {
	return video.NewSession(device, captureWidth, captureHeight, width, height, fps, captureFPS)
}

type Model struct {
	choices       []string
	devices       []video.Device
	cursor        int
	selected      int
	loading       bool
	hideUI        bool
	err           error
	screen        screenState
	termWidth     int
	termHeight    int
	color         bool
	paletteMode   string
	customPalette string
	fps           int
	textInput     textinput.Model
	inputActive   bool
	deviceList    list.Model
	mirror        bool
	ratio         float64

	// session is the single owner of everything tied to one visit to the
	// camera view. It is replaced on every entry and closed on every exit.
	session    *cameraSession
	sessionSeq uint64
	// playbackRestore holds the user's own settings while playback overrides
	// them from the recording header.
	playbackRestore *userSettings
}

// userSettings is the subset of settings a recording header can override.
type userSettings struct {
	fps         int
	color       bool
	paletteMode string
	mirror      bool
	ratio       float64
}

// Frame capture message types. Every frame result carries the id of the session
// that produced it so late results from a closed session can be discarded.
type frameMsg struct{ sessionID uint64 }
type frameErrMsg struct {
	sessionID uint64
	err       error
}

type devicesLoadedMsg []video.Device
type errMsg struct{ err error }

// List item for video devices
type deviceItem struct {
	device   video.Device
	selected bool
}

func (i deviceItem) Title() string {
	if i.selected {
		return "[x] " + i.device.String()
	}
	return "[ ] " + i.device.String()
}

func (i deviceItem) Description() string {
	return i.device.ID
}

func (i deviceItem) FilterValue() string {
	return i.device.String()
}

// placeholderItem serves as a visual indicator at the end of the list
type placeholderItem struct{}

func (p placeholderItem) Title() string {
	return "• (No other devices detected)"
}

func (p placeholderItem) Description() string {
	return "This is the end of the available video devices list."
}

func (p placeholderItem) FilterValue() string {
	return "No other devices detected"
}

func (m *Model) updateListItems() {
	items := make([]list.Item, 0, len(m.devices)+1)
	for i, d := range m.devices {
		items = append(items, deviceItem{
			device:   d,
			selected: m.selected == i,
		})
	}

	items = append(items, placeholderItem{})

	m.deviceList.SetItems(items)
}

// InitialModel returns the model for the process command line arguments.
func InitialModel() Model {
	return initialModel(cliArgs[1:])
}

// initialModel builds the model for the given arguments. The first argument,
// when present, is a .tcam recording to play back.
func initialModel(args []string) Model {
	saved := LoadSettings()

	ti := textinput.New()
	ti.Placeholder = "30"
	ti.SetValue("30")
	ti.CharLimit = 3
	ti.SetWidth(5)

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.Foreground(magenta).BorderLeftForeground(magenta)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.Foreground(gray).BorderLeftForeground(magenta)

	l := list.New(nil, delegate, 0, 0)
	l.Title = "Select video devices to configure"
	l.SetShowHelp(false)
	m := Model{
		selected:      -1,
		loading:       true,
		hideUI:        false,
		color:         saved.Color,
		paletteMode:   saved.PaletteMode,
		customPalette: saved.CustomPalette,
		mirror:        saved.Mirror,
		fps:           saved.FPS,
		textInput:     ti,
		inputActive:   false,
		deviceList:    l,
		ratio:         saved.Ratio,
	}

	// Detect if a recording file is being given as argument
	if len(args) > 0 && args[0] != "" {
		filePath := args[0]
		if err := m.enterPlaybackSession(filePath); err != nil {
			m.err = fmt.Errorf("failed to load recording %q: %w", filePath, err)
		} else {
			m.screen = screenCamera
			m.loading = false
		}
	}

	return m
}

func (m Model) Init() tea.Cmd {
	if m.session != nil && m.session.playback {
		return playbackTickCmd(m.session.id, m.fps)
	}
	return getDevicesCmd
}

func getDevicesCmd() tea.Msg {
	devices, err := video.ListDevices()
	if err != nil {
		return errMsg{err}
	}

	return devicesLoadedMsg(devices)
}

func (m Model) getPalette() string {
	switch m.paletteMode {
	case "detailed":
		return ascii.PaletteDetailed
	case "custom":
		if m.customPalette != "" {
			return m.customPalette
		}
		return ascii.PaletteSimple
	default:
		return ascii.PaletteSimple
	}
}

// isPlayback reports whether the active session is replaying a recording.
func (m Model) isPlayback() bool {
	return m.session != nil && m.session.playback
}

// isRecording reports whether the active session is writing a recording.
func (m Model) isRecording() bool {
	return m.session.isRecording()
}

// snapshotSettings captures the user's own settings before playback overrides
// them from a recording header.
func (m Model) snapshotSettings() userSettings {
	return userSettings{
		fps:         m.fps,
		color:       m.color,
		paletteMode: m.paletteMode,
		mirror:      m.mirror,
		ratio:       m.ratio,
	}
}

// restoreUserSettings undoes any playback overrides.
func (m *Model) restoreUserSettings() {
	if m.playbackRestore == nil {
		return
	}
	m.fps = m.playbackRestore.fps
	m.color = m.playbackRestore.color
	m.paletteMode = m.playbackRestore.paletteMode
	m.mirror = m.playbackRestore.mirror
	m.ratio = m.playbackRestore.ratio
	m.playbackRestore = nil
}

func (m *Model) nextSessionID() uint64 {
	m.sessionSeq++
	return m.sessionSeq
}

// closeSession stops and releases the active session. It is idempotent and
// safe to call when no session is active; closing stops the frame source (which
// unblocks any in-flight read and reaps the FFmpeg child process), stops any
// recording in progress and restores playback-overridden settings.
func (m *Model) closeSession() {
	if m.session == nil {
		m.restoreUserSettings()
		return
	}

	session := m.session
	m.session = nil
	session.close()

	m.restoreUserSettings()
}

// enterLiveSession probes the device and starts a fresh capture session sized to
// the frame dimensions of this entry. Any previous session is closed first so a
// failed entry cannot leave a stale session behind.
func (m *Model) enterLiveSession(device video.Device) error {
	nativeW, nativeH, err := probeResolution(device)
	if err != nil || nativeW <= 0 || nativeH <= 0 {
		nativeW, nativeH = 640, 480 // Fallback
	}

	// Calculate aspect ratio
	aspectRatio := float64(nativeW) / float64(nativeH)

	// Cap maximum width to 640px to keep CPU load low
	maxWidth := 640
	if nativeW < maxWidth {
		maxWidth = nativeW
	}

	// Scale height proportionally to maintain exact aspect ratio
	width := maxWidth
	height := int(float64(maxWidth) / aspectRatio)
	if height <= 0 {
		height = 1
	}

	captureFPS, err := probeFramerate(device, nativeW, nativeH, m.fps)
	if err != nil || captureFPS <= 0 {
		captureFPS = 30
	}

	// Tear the previous session down before creating the next one.
	m.closeSession()

	source, err := openLiveSource(device, nativeW, nativeH, width, height, m.fps, captureFPS)
	if err != nil {
		return err
	}

	m.session = newLiveCameraSession(m.nextSessionID(), source, width, height)

	return nil
}

// enterPlaybackSession opens a recording and starts a playback session whose
// buffers, frame rate and palette are bound to the file header.
func (m *Model) enterPlaybackSession(path string) error {
	reader, err := openPlayback(path)
	if err != nil {
		return err
	}

	// Drop any previous session before overriding the settings.
	m.closeSession()
	restore := m.snapshotSettings()

	session := newPlaybackCameraSession(m.nextSessionID(), reader)

	// Read the first frame up front so entering the view never renders a blank
	// buffer. The read is done on the new session only; a failure closes the
	// file and decompressor via the reader.
	if err := session.source.ReadFrame(session.back); err != nil {
		_ = reader.Close()
		return fmt.Errorf("failed to read initial frame: %w", err)
	}
	session.swapBuffers()

	m.session = session
	m.playbackRestore = &restore

	// Settings come from the file header while playback is active.
	m.fps = int(session.header.FPS)
	m.color = byteToBool(session.header.Color)
	if byteToBool(session.header.Detailed) {
		m.paletteMode = "detailed"
	} else {
		m.paletteMode = "default"
	}
	m.mirror = byteToBool(session.header.Mirror)
	if session.header.hasRatio() {
		m.ratio = session.header.Ratio
	}

	return nil
}

// recordingFilename is a seam so tests can direct recordings into a temp dir.
var recordingFilename = func() string {
	return fmt.Sprintf("rec_%s.tcam", time.Now().Format("2006-01-02_15-04-05"))
}

// startRecording opens a recording file bound to the active session.
func (m *Model) startRecording(path string) error {
	if m.session == nil {
		return errors.New("no active camera session")
	}
	if m.session.playback {
		return errors.New("recording is not available during playback")
	}

	header := fileHeader{
		Width:    int32(m.session.width),  // #nosec G115 -- Safe. Dimensions are small terminal resolutions
		Height:   int32(m.session.height), // #nosec G115 -- Safe. Dimensions are small terminal resolutions
		FPS:      int32(m.fps),            // #nosec G115 -- Safe. Target FPS is validated and low
		Color:    boolToByte(m.color),
		Detailed: boolToByte(m.paletteMode == "detailed"),
		Mirror:   boolToByte(m.mirror),
		Ratio:    m.ratio,
	}

	return m.session.startRecording(path, header)
}

// stopRecording finishes the active recording, if any.
func (m *Model) stopRecording() {
	if m.session != nil {
		_ = m.session.stopRecording()
	}
}
