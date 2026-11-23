package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lancekrogers/peep/internal/browser"
	"github.com/lancekrogers/peep/internal/document"
	"github.com/lancekrogers/peep/internal/media"
	"github.com/lancekrogers/peep/internal/player"
	"github.com/lancekrogers/peep/internal/render"
)

type nativeMsg struct {
	id  int
	png []byte
	err error
}

// Frame decoders are indirected so tests can substitute in-memory streams.
var decodeFramesAtFPS = media.FramesAtFPS
var decodeNativeFramesAtFPS = media.NativeFramesAtFPS

type tickMsg time.Time
type listMsg struct {
	id      int
	entries []browser.Entry
	err     error
}
type previewMsg struct {
	id   int
	info media.Info
	err  error
}
type frameMsg struct {
	id    int
	frame media.Frame
	ok    bool
}
type loadedMsg struct {
	id   int
	info media.Info
	err  error
}
type controlMsg struct {
	err  error
	seek *float64
}

type Model struct {
	terminalInput                             *os.File
	resultsMode                               bool
	resultPaths                               []string
	resultBase, resultRoot, resultSelected    string
	targetFPS                                 int
	videoStarted                              time.Time
	frameTimes                                []time.Time
	navCount                                  int
	navG                                      bool
	relativeNumbers                           bool
	filePrompt                                *filePrompt
	editing                                   bool
	docImages                                 map[int]*documentImage
	docImageCtx                               context.Context
	docImageCancel                            context.CancelFunc
	docImageRevision                          int
	docMermaid                                *document.Mermaid
	doc                                       *document.Document
	docLayout                                 document.Layout
	docOffset                                 int
	docSearch                                 documentSearch
	docSource                                 bool
	stopPending                               bool
	seekStep                                  time.Duration
	spectrum                                  <-chan media.SpectrumFrame
	spectrumCancel                            context.CancelFunc
	spectrumID                                int
	spectrumPending                           *media.SpectrumFrame
	bands, bandTarget                         [media.SpectrumBands]float64
	spectrumError                             string
	startState                                string
	graphics                                  *render.KittyWriter
	nativePNG                                 []byte
	nativeRevision                            int
	previewError                              string
	timeline                                  bool
	ctx                                       context.Context
	backend                                   *player.Player
	dir                                       string
	entries                                   []browser.Entry
	allEntries                                []browser.Entry
	sortField                                 browser.SortField
	sortDescending, sortExplicit, sortMenu    bool
	browserOnly                               bool
	cursor, width, height                     int
	helpOffset                                int
	hidden, filtering, focus, help, showQueue bool
	filter, notice, state                     string
	history                                   map[string]string
	wanted                                    string
	listID, previewID, loadID                 int
	previewCancel, loadCancel                 context.CancelFunc
	preview                                   media.Info
	rows                                      []string
	frames                                    <-chan media.Frame
	pending                                   *media.Frame
	displayingActive                          bool
	queue                                     []string
	queueCursor, index                        int
	active                                    media.Info
	position, duration, speed, volume, hold   float64
	muted                                     bool
	repeat                                    int
	busy                                      bool
	last                                      time.Time
}

func New(ctx context.Context, p *player.Player, paths []string, play bool, notice string) (*Model, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	m := &Model{ctx: ctx, backend: p, dir: cwd, width: 100, height: 30, state: "Empty", speed: 1, volume: 70, hold: 5, index: -1, history: map[string]string{}, notice: notice, last: time.Now()}
	if len(paths) > 0 {
		for i, path := range paths {
			abs, e := filepath.Abs(path)
			if e != nil {
				return nil, e
			}
			paths[i] = abs
		}
		st, e := os.Stat(paths[0])
		if e != nil {
			return nil, e
		}
		if st.IsDir() {
			if len(paths) > 1 {
				return nil, fmt.Errorf("pass a directory or a list of media files")
			}
			m.dir = paths[0]
		} else {
			m.dir = filepath.Dir(paths[0])
			m.wanted = paths[0]
		}
	}
	if play {
		for _, path := range paths {
			st, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if !st.IsDir() && (!st.Mode().IsRegular() || media.Classify(path) == "") {
				return nil, fmt.Errorf("not playable media: %s (omit --play to preview)", filepath.Base(path))
			}
		}
		m.queue = append([]string(nil), paths...)
		if len(paths) == 0 {
			m.queue = nil
		}
		if len(paths) == 1 {
			if st, _ := os.Stat(paths[0]); st != nil && st.IsDir() {
				m.queue = nil
			}
		}
		m.index = -2
	}
	return m, nil
}
func (m *Model) tick() tea.Cmd {
	interval := time.Second / 20
	if m.clockedVideo() && m.state == "Playing" {
		// Schedule against the media clock more frequently than the requested
		// frames, instead of silently limiting 24/30/60 FPS video to 20 FPS.
		interval = time.Second / 60
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *Model) Init() tea.Cmd { return tea.Batch(m.readDir(), m.tick()) }
func (m *Model) readDir() tea.Cmd {
	m.listID++
	id, path, hidden := m.listID, m.dir, m.hidden
	if m.resultsMode {
		paths, base := append([]string(nil), m.resultPaths...), m.resultBase
		return func() tea.Msg { entries, err := browser.ReadPaths(paths, base); return listMsg{id, entries, err} }
	}
	return func() tea.Msg { entries, err := browser.Read(path, hidden, ""); return listMsg{id, entries, err} }
}
func (m *Model) selected() *browser.Entry {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return nil
	}
	return &m.entries[m.cursor]
}
func (m *Model) previewSelection() tea.Cmd {
	if e := m.selected(); e != nil && m.doc != nil && m.doc.Path == e.Path && !m.displayingActive {
		return m.reflowDocument()
	}
	m.clearDocument()
	if m.previewCancel != nil {
		m.previewCancel()
	}
	m.previewID++
	m.frames = nil
	m.pending = nil
	m.rows = nil
	m.nativePNG = nil
	m.displayingActive = false
	m.previewError = ""
	entry := m.selected()
	if entry == nil || entry.Dir {
		m.preview = media.Info{}
		if !m.transportVisible() {
			m.timeline = false
		}
		return nil
	}
	m.preview = media.Info{Path: entry.Path, Kind: entry.Kind}
	if !m.transportVisible() {
		m.timeline = false
	}
	if entry.Kind == "" {
		return m.readDocument(entry.Path)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.previewCancel = cancel
	id, path := m.previewID, entry.Path
	return func() tea.Msg { info, err := media.Probe(ctx, path); return previewMsg{id, info, err} }
}
func (m *Model) dimensions() (int, int) {
	l := m.layout()
	w := l.preview
	if w == 0 {
		w = m.width - 2
	}
	return max(4, min(w-2, 120)), max(2, min(l.body*2, 100))
}
func (m *Model) startFrames(info media.Info, start float64, active bool) tea.Cmd {
	m.clearDocument()
	if m.previewCancel != nil {
		m.previewCancel()
	}
	m.previewID++
	m.frames = nil
	m.pending = nil
	m.rows = nil
	m.nativePNG = nil
	m.displayingActive = active
	m.videoStarted = time.Now()
	m.frameTimes = nil
	m.preview = info
	m.previewError = ""
	if active && info.Kind == media.Audio {
		return nil
	}
	if !info.HasVisual {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.previewCancel = cancel
	if m.graphics != nil && (!active || info.Kind == media.Image || info.Kind == media.Audio) {
		id, path := m.previewID, info.Path
		return func() tea.Msg { png, err := media.PreviewPNG(ctx, path); return nativeMsg{id, png, err} }
	}
	if m.graphics != nil {
		m.frames = decodeNativeFramesAtFPS(ctx, info.Path, start, float64(m.videoFPS()))
	} else {
		w, h := m.dimensions()
		m.frames = decodeFramesAtFPS(ctx, info.Path, w, h, start, !active || info.Kind == media.Image || info.Kind == media.Audio, float64(m.videoFPS()))
	}
	return m.nextFrame()
}
func (m *Model) nextFrame() tea.Cmd {
	ch, id := m.frames, m.previewID
	if ch == nil {
		return nil
	}
	return func() tea.Msg { f, ok := <-ch; return frameMsg{id, f, ok} }
}
func (m *Model) showActive() tea.Cmd {
	if m.active.Path == "" {
		return nil
	}
	return m.startFrames(m.active, m.position, true)
}
func (m *Model) queueFromSelection() tea.Cmd {
	e := m.selected()
	if e == nil || e.Dir || e.Kind == "" {
		return nil
	}
	m.queue = nil
	idx := 0
	for _, entry := range m.entries {
		if !entry.Dir && entry.Kind != "" {
			if entry.Path == e.Path {
				idx = len(m.queue)
			}
			m.queue = append(m.queue, entry.Path)
		}
	}
	return m.load(idx)
}
func (m *Model) load(index int) tea.Cmd { return m.loadState(index, "Playing") }
func (m *Model) loadState(index int, desired string) tea.Cmd {
	if m.busy || index < 0 || index >= len(m.queue) {
		return nil
	}
	if m.doc == nil {
		if m.previewCancel != nil {
			m.previewCancel()
		}
		m.frames = nil
		m.pending = nil
		m.rows = nil
		m.nativePNG = nil
		m.previewError = ""
		m.previewID++
	}
	m.clearSpectrum()
	m.index = index
	m.queueCursor = index
	m.loadID++
	m.busy = true
	m.state = "Loading"
	m.startState = desired
	m.position = 0
	m.duration = 0
	m.active = media.Info{Path: m.queue[index]}
	m.notice = ""
	ctx, cancel := context.WithCancel(m.ctx)
	m.loadCancel = cancel
	id, path, p, speed := m.loadID, m.queue[index], m.backend, m.speed
	return func() tea.Msg {
		if p != nil {
			if err := p.Pause(ctx, true); err != nil {
				return loadedMsg{id, media.Info{}, err}
			}
		}
		info, err := media.Probe(ctx, path)
		if err == nil && info.Kind != media.Image {
			if p == nil {
				err = fmt.Errorf("mpv is required for GIF, video, and music playback; run peep --doctor")
			} else {
				err = p.Load(ctx, path)
				if err == nil {
					err = p.Command(ctx, "set_property", "speed", speed)
				}
			}
		}
		if err == nil && info.Kind != media.Image {
			err = p.Pause(ctx, desired != "Playing")
		}
		return loadedMsg{id, info, err}
	}
}
func (m *Model) control(fn func(context.Context, *player.Player) error, seek *float64) tea.Cmd {
	if m.busy || m.backend == nil {
		return nil
	}
	m.busy = true
	p, ctx := m.backend, m.ctx
	return func() tea.Msg { return controlMsg{fn(ctx, p), seek} }
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case fileOperationMsg:
		return m, m.applyFileOperation(v)
	case editorDoneMsg:
		return m, m.editorFinished(v)
	case documentImageMsg:
		return m, m.applyDocumentImage(v)
	case documentMsg:
		if v.id == m.previewID {
			m.applyDocument(v)
			return m, m.requestDocumentImages()
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = max(1, v.Width)
		m.height = max(1, v.Height)
		if m.displayingActive {
			return m, m.showActive()
		}
		return m, m.previewSelection()
	case listMsg:
		if v.id != m.listID {
			return m, nil
		}
		if v.err != nil {
			m.notice = v.err.Error()
			return m, nil
		}
		m.allEntries = v.entries
		m.entries = browser.Filter(v.entries, m.filter)
		m.sortEntries()
		if !m.resultsMode && m.filter == "" && filepath.Dir(m.dir) != m.dir {
			m.entries = append([]browser.Entry{{Path: filepath.Dir(m.dir), Name: "..", Dir: true}}, m.entries...)
		}
		m.cursor = 0
		if len(m.entries) > 1 && m.entries[0].Name == ".." {
			m.cursor = 1
		}
		want := m.wanted
		if want == "" {
			want = m.history[m.dir]
		}
		for i, e := range m.entries {
			if e.Path == want {
				m.cursor = i
				break
			}
		}
		m.wanted = ""
		if m.index == -2 {
			m.index = -1
			if len(m.queue) > 0 {
				return m, m.load(0)
			}
			if e := m.selected(); e == nil || e.Dir || e.Kind == "" {
				for i, entry := range m.entries {
					if !entry.Dir && entry.Kind != "" {
						m.cursor = i
						break
					}
				}
			}
			return m, m.queueFromSelection()
		}
		return m, m.previewSelection()
	case previewMsg:
		if v.id != m.previewID {
			return m, nil
		}
		if v.err != nil {
			m.notice = v.err.Error()
			m.previewError = v.err.Error()
			return m, nil
		}
		m.notice = ""
		return m, m.startFrames(v.info, 0, false)
	case spectrumMsg:
		if v.id != m.spectrumID {
			return m, nil
		}
		if !v.ok {
			m.spectrum = nil
			return m, nil
		}
		if v.frame.Err != nil {
			m.spectrumError = v.frame.Err.Error()
			m.spectrum = nil
			return m, nil
		}
		if v.frame.Time <= m.position+0.04 {
			m.bandTarget = v.frame.Levels
			if m.state == "Paused" {
				m.bands = v.frame.Levels
			}
			return m, m.nextSpectrum()
		}
		m.spectrumPending = &v.frame
	case nativeMsg:
		if v.id != m.previewID {
			return m, nil
		}
		if v.err != nil {
			m.previewError = v.err.Error()
			m.notice = v.err.Error()
			return m, nil
		}
		m.nativePNG = v.png
		m.nativeRevision++
	case frameMsg:
		if v.id != m.previewID {
			return m, nil
		}
		if !v.ok {
			m.frames = nil
			return m, nil
		}
		if v.frame.Err != nil {
			m.notice = v.frame.Err.Error()
			m.previewError = v.frame.Err.Error()
			m.frames = nil
			return m, nil
		}
		if m.clockedVideo() {
			if m.backend != nil && m.state == "Playing" {
				m.position = m.backend.Snapshot().Position
			}
			if m.position-v.frame.Time > m.frameTolerance() {
				if m.state == "Playing" && m.position-v.frame.Time > 1 && time.Since(m.videoStarted) > 2*time.Second {
					return m, m.restartVideoFrames()
				}
				return m, m.nextFrame()
			}
		}
		if !m.displayingActive || m.active.Kind == media.Image || m.active.Kind == media.Audio || v.frame.Time <= m.position+0.02 {
			m.showFrame(v.frame)
			return m, m.nextFrame()
		}
		m.pending = &v.frame
	case loadedMsg:
		if v.id != m.loadID {
			return m, nil
		}
		m.busy = false
		if v.err != nil {
			m.state = "Error"
			m.notice = v.err.Error()
			return m, nil
		}
		m.active = v.info
		m.duration = v.info.Duration
		if v.info.Kind == media.Image {
			m.duration = m.hold
		}
		m.state = m.startState
		m.last = time.Now()
		if m.doc != nil && !m.displayingActive {
			return m, m.startSpectrum()
		}
		return m, tea.Batch(m.showActive(), m.startSpectrum())
	case controlMsg:
		m.busy = false
		if m.stopPending {
			m.stopPending = false
			return m, m.stop()
		}
		if v.err != nil {
			m.state = "Error"
			m.notice = v.err.Error()
			return m, nil
		}
		if v.seek != nil {
			m.position = *v.seek
			cmd := m.startSpectrum()
			if m.displayingActive {
				return m, tea.Batch(m.showActive(), cmd)
			}
			return m, cmd
		}
	case tickMsg:
		hadTransport := m.transportVisible()
		now := time.Time(v)
		dt := now.Sub(m.last).Seconds()
		m.last = now
		if m.state == "Playing" && !m.busy {
			if m.active.Kind == media.Image {
				m.position += dt * m.speed
			} else if m.backend != nil {
				snap := m.backend.Snapshot()
				m.position = snap.Position
				if snap.Duration > 0 {
					m.duration = snap.Duration
				}
				if snap.Error != "" {
					m.state = "Error"
					m.notice = snap.Error
				}
				if snap.Ended {
					m.position = m.duration
					m.state = "Ended"
				}
			}
			if m.duration > 0 && m.position >= m.duration {
				m.position = m.duration
				m.state = "Ended"
				if m.repeat == 1 {
					return m, tea.Batch(m.tick(), m.load(m.index))
				}
				if m.index+1 < len(m.queue) {
					return m, tea.Batch(m.tick(), m.load(m.index+1))
				}
				if m.repeat == 2 && len(m.queue) > 0 {
					return m, tea.Batch(m.tick(), m.load(0))
				}
			}
		}
		cmds := []tea.Cmd{m.tick()}
		if hadTransport != m.transportVisible() {
			m.timeline = false
			cmds = append(cmds, m.requestDocumentImages())
		}
		if m.state == "Playing" {
			for i := range m.bands {
				target := m.bandTarget[i]
				if target > m.bands[i] {
					m.bands[i] = target
				} else {
					m.bands[i] = max(target, m.bands[i]-dt*1.8)
				}
			}
		} else if m.state == "Ended" || m.state == "Stopped" {
			m.bands = [media.SpectrumBands]float64{}
		}
		if m.spectrumPending != nil && m.spectrumPending.Time <= m.position+0.04 {
			m.bandTarget = m.spectrumPending.Levels
			m.spectrumPending = nil
			cmds = append(cmds, m.nextSpectrum())
		}
		if m.pending != nil && m.pending.Time <= m.position+0.02 {
			if !m.clockedVideo() || m.position-m.pending.Time <= m.frameTolerance() {
				m.showFrame(*m.pending)
			}
			m.pending = nil
			cmds = append(cmds, m.nextFrame())
		}
		return m, tea.Batch(cmds...)

	case tea.MouseMsg:
		return m, m.mouse(v)
	case tea.KeyMsg:
		if v.Type == tea.KeyRunes && len(v.Runes) > 1 && !v.Paste {
			cmds := []tea.Cmd{}
			for _, r := range v.Runes {
				cmds = append(cmds, m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
			}
			return m, tea.Batch(cmds...)
		}
		if v.Paste && !m.filtering && !m.docSearch.editing {
			return m, nil
		}
		return m, m.key(v)
	}
	return m, nil
}

func (m *Model) SetGraphics(writer *render.KittyWriter) { m.graphics = writer }
func (m *Model) syncGraphics() {
	if m.graphics == nil {
		return
	}
	l := m.layout()
	x := 2
	if l.files > 0 {
		x += l.files + 1
	}
	visible := !m.help && m.filePrompt == nil && !m.editing && l.preview > 0 && m.width >= 35 && m.height >= 12
	if m.doc != nil {
		if visible {
			m.graphics.SetImages(m.documentGraphics(x, 3))
		} else {
			m.graphics.SetImages(nil)
		}
		return
	}
	m.graphics.Set(m.nativePNG, m.nativeRevision, render.Placement{X: x, Y: 3, Columns: l.preview - 2, Rows: l.body}, visible)
}

func (m *Model) refilter() tea.Cmd {
	path := ""
	if e := m.selected(); e != nil {
		path = e.Path
	}
	m.entries = browser.Filter(m.allEntries, m.filter)
	m.sortEntries()
	if !m.resultsMode && m.filter == "" && filepath.Dir(m.dir) != m.dir {
		m.entries = append([]browser.Entry{{Path: filepath.Dir(m.dir), Name: "..", Dir: true}}, m.entries...)
	}
	m.cursor = 0
	if m.filter == "" && len(m.entries) > 1 && m.entries[0].Name == ".." {
		m.cursor = 1
	}
	for i, e := range m.entries {
		if e.Path == path {
			m.cursor = i
			break
		}
	}
	return m.previewSelection()
}

func (m *Model) showFrame(f media.Frame) {
	m.frameTimes = append(m.frameTimes, time.Now())
	m.frameTimes = recentFrames(m.frameTimes, time.Now())
	if len(f.PNG) > 0 {
		m.nativePNG = f.PNG
		m.nativeRevision++
		m.rows = nil
	} else {
		m.rows = render.Cells(f)
	}
}
