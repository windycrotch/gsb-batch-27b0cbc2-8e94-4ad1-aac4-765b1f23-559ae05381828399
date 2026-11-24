package xm

import (
	"io"
	"sync"
	"testing"

	"github.com/quasilyte/xm/xmfile"
)

// testInstrument builds a single-sample instrument.
// If withEnvelope is true, volume/panning envelopes are enabled,
// so the envelope runtime state can be observed during the playback.
func testInstrument(volume, panning uint8, withEnvelope bool) xmfile.Instrument {
	data := make([]uint8, 512)
	for i := range data {
		data[i] = uint8((i * 7) % 31)
	}
	inst := xmfile.Instrument{
		Samples: []xmfile.InstrumentSample{{
			Length:  len(data),
			Volume:  int(volume),
			Panning: panning,
			Data:    data,
		}},
	}
	if withEnvelope {
		inst.EnvelopeVolume = []xmfile.EnvelopePoint{{X: 0, Y: 64}, {X: 200, Y: 0}}
		inst.VolumeFlags = 1 // enabled
		inst.EnvelopePanning = []xmfile.EnvelopePoint{{X: 0, Y: 32}, {X: 200, Y: 32}}
		inst.PanningFlags = 1 // enabled
	}
	return inst
}

// testModule builds a single-pattern module.
// rows[y][x] is an index into the notes slice for channel x of row y.
func testModule(numChannels, tempo, bpm int, instruments []xmfile.Instrument, notes []xmfile.PatternNote, rows [][]uint16) *xmfile.Module {
	patRows := make([]xmfile.PatternRow, len(rows))
	for i, r := range rows {
		if len(r) != numChannels {
			panic("bad test module row")
		}
		patRows[i] = xmfile.PatternRow{Notes: r}
	}
	return &xmfile.Module{
		Name:           "test",
		Version:        [2]byte{1, 4},
		SongLength:     1,
		NumChannels:    numChannels,
		NumPatterns:    1,
		NumInstruments: len(instruments),
		Flags:          1, // linear frequency table
		DefaultTempo:   tempo,
		DefaultBPM:     bpm,
		PatternOrder:   []uint8{0},
		Patterns:       []xmfile.Pattern{{Rows: patRows}},
		Notes:          notes,
		Instruments:    instruments,
	}
}

// testModuleA returns a 2-channels module that dirties a lot of the
// playback state: global volume, BPM, tempo, channel panning,
// volume slide, vibrato and envelope progress.
func testModuleA() *xmfile.Module {
	notes := []xmfile.PatternNote{
		// 0: normal note + set global volume to 0.25.
		{Note: 48, Instrument: 1, EffectType: 0x10, EffectParameter: 0x10},
		// 1: set BPM to 200.
		{EffectType: 0x0F, EffectParameter: 200},
		// 2: set tempo (ticks per row) to 10.
		{EffectType: 0x0F, EffectParameter: 10},
		// 3: normal note + set panning to 1.0.
		{Note: 50, Instrument: 1, EffectType: 0x08, EffectParameter: 0xFF},
		// 4: volume slide down.
		{EffectType: 0x0A, EffectParameter: 0x01},
		// 5: normal note + vibrato.
		{Note: 49, Instrument: 1, EffectType: 0x04, EffectParameter: 0x44},
	}
	rows := [][]uint16{
		{0, 3},
		{1, 4},
		{2, 5},
		{0, 3},
	}
	instruments := []xmfile.Instrument{
		testInstrument(64, 128, true),
	}
	return testModule(2, 6, 120, instruments, notes, rows)
}

// testModuleB returns a simple 1-channel module with different
// tempo/BPM defaults than testModuleA.
func testModuleB() *xmfile.Module {
	notes := []xmfile.PatternNote{
		{Note: 48, Instrument: 1},
	}
	rows := [][]uint16{
		{0},
		{0},
	}
	instruments := []xmfile.Instrument{
		testInstrument(64, 128, false),
	}
	return testModule(1, 8, 140, instruments, notes, rows)
}

func drainStream(t *testing.T, r io.Reader) (total int, nonzero bool) {
	t.Helper()
	buf := make([]byte, 8192)
	for i := 0; i < 1000; i++ {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				nonzero = true
				break
			}
		}
		total += n
		if err == io.EOF {
			return total, nonzero
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
		if n == 0 {
			t.Fatalf("read made no progress")
		}
	}
	t.Fatalf("stream did not reach EOF")
	return 0, false
}

func bytesPerTickForBPM(bpm float64) int {
	_, bytesPerTick := calcSamplesPerTick(44100, bpm)
	return bytesPerTick
}

// assertCleanState checks that the stream is at the very beginning
// of its module with no leftovers from a previous playback.
func assertCleanState(t *testing.T, s *Stream, tempo int, bpm float64) {
	t.Helper()

	if s.globalVolume != 1.0 {
		t.Errorf("globalVolume: got %v, want 1.0", s.globalVolume)
	}
	if s.bpm != bpm {
		t.Errorf("bpm: got %v, want %v", s.bpm, bpm)
	}
	if s.ticksPerRow != tempo {
		t.Errorf("ticksPerRow: got %v, want %v", s.ticksPerRow, tempo)
	}
	if s.t != 0 {
		t.Errorf("t: got %v, want 0", s.t)
	}
	if s.bytePos != 0 {
		t.Errorf("bytePos: got %v, want 0", s.bytePos)
	}
	if s.patternIndex != -1 || s.patternRowIndex != -1 {
		t.Errorf("pattern pos: got (%v, %v), want (-1, -1)", s.patternIndex, s.patternRowIndex)
	}
	if s.jumpKind != jumpNone {
		t.Errorf("jumpKind: got %v, want jumpNone", s.jumpKind)
	}
	if len(s.activeChannels) != 0 {
		t.Errorf("activeChannels: got %d, want 0", len(s.activeChannels))
	}
	for i := range s.channels {
		ch := &s.channels[i]
		if ch.inst != nil {
			t.Errorf("ch[%d].inst: got %v, want nil", i, ch.inst)
		}
		if ch.volume != 0 || ch.panning != 0 {
			t.Errorf("ch[%d] volume/panning: got (%v, %v), want (0, 0)", i, ch.volume, ch.panning)
		}
		if ch.volumeEnvelope.frame != 0 || ch.volumeEnvelope.value != 0 {
			t.Errorf("ch[%d] volume envelope: got frame=%v value=%v, want 0/0",
				i, ch.volumeEnvelope.frame, ch.volumeEnvelope.value)
		}
		if ch.panningEnvelope.frame != 0 {
			t.Errorf("ch[%d] panning envelope frame: got %v, want 0", i, ch.panningEnvelope.frame)
		}
		if ch.volumeSlideValue != 0 || ch.panningSlideValue != 0 || ch.globalVolumeSlideValue != 0 {
			t.Errorf("ch[%d] slide values are not reset", i)
		}
		if ch.vibratoRunning || ch.vibratoDepth != 0 || ch.vibratoStep != 0 || ch.vibratoPeriodOffset != 0 {
			t.Errorf("ch[%d] vibrato state is not reset", i)
		}
		if ch.arpeggioRunning || ch.arpeggioNoteOffset != 0 {
			t.Errorf("ch[%d] arpeggio state is not reset", i)
		}
		if ch.keyOn {
			t.Errorf("ch[%d] keyOn: got true, want false", i)
		}
	}
}

func TestStreamReloadResetsState(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}

	// Play the entire module A; its effects dirty the playback state.
	if _, nonzero := drainStream(t, s); !nonzero {
		t.Fatalf("module A produced only silence")
	}

	// Sanity check: the state is really dirty now.
	if s.globalVolume == 1.0 {
		t.Fatalf("globalVolume was not changed by module A effects")
	}
	if s.bpm == 120 {
		t.Fatalf("bpm was not changed by module A effects")
	}
	if s.ticksPerRow == 6 {
		t.Fatalf("ticksPerRow was not changed by module A effects")
	}
	if s.channels[1].panning != 1.0 {
		t.Fatalf("ch[1] panning was not changed by module A effects")
	}
	if s.channels[1].volumeSlideValue == 0 {
		t.Fatalf("ch[1] volume slide was not changed by module A effects")
	}
	if s.channels[1].vibratoDepth == 0 {
		t.Fatalf("ch[1] vibrato was not changed by module A effects")
	}
	if s.channels[0].volumeEnvelope.frame == 0 {
		t.Fatalf("ch[0] volume envelope did not progress")
	}

	// Loading another module must reset everything to the new module defaults.
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module B: %v", err)
	}

	if len(s.channels) != 1 {
		t.Fatalf("channels count: got %d, want 1", len(s.channels))
	}
	assertCleanState(t, s, 8, 140)

	// The stream position and info should be consistent with the new module.
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Errorf("byte pos after reload: got %d, want 0", pos)
	}
	if info := s.GetInfo(); info.BytesPerTick != uint(bytesPerTickForBPM(140)) {
		t.Errorf("BytesPerTick after reload: got %d, want %d", info.BytesPerTick, bytesPerTickForBPM(140))
	}

	// The new module must be playable from its start.
	n, nonzero := drainStream(t, s)
	if n == 0 || !nonzero {
		t.Fatalf("module B is not playable after reload (n=%d, nonzero=%v)", n, nonzero)
	}
}

func TestStreamRewindResetsState(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}

	if _, nonzero := drainStream(t, s); !nonzero {
		t.Fatalf("module A produced only silence")
	}
	if s.globalVolume == 1.0 || s.bpm == 120 || s.ticksPerRow == 6 {
		t.Fatalf("module A effects did not dirty the state")
	}

	s.Rewind()

	assertCleanState(t, s, 6, 120)

	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Errorf("byte pos after rewind: got %d, want 0", pos)
	}

	// After a rewind the module plays from the beginning again.
	n, nonzero := drainStream(t, s)
	if n == 0 || !nonzero {
		t.Fatalf("module A is not playable after rewind (n=%d, nonzero=%v)", n, nonzero)
	}
}

func TestStreamLoadFailureKeepsState(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}

	// Play a bit, then snapshot the state.
	buf := make([]byte, 8192)
	if _, err := s.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	posBefore, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if posBefore == 0 {
		t.Fatalf("no bytes were produced")
	}
	bpmBefore := s.bpm
	numChannelsBefore := len(s.channels)
	infoBefore := s.GetInfo()

	// Unsupported sample rate must fail without touching the stream.
	wideModule := testModuleB() // different channel count on purpose
	if err := s.LoadModule(wideModule, LoadModuleConfig{SampleRate: 22050}); err == nil {
		t.Fatalf("expected an error for unsupported sample rate")
	}
	// Amiga frequency table is not supported: a compilation error.
	amigaModule := testModuleA()
	amigaModule.Flags = 0
	if err := s.LoadModule(amigaModule, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected an error for amiga frequency table")
	}
	// A module with a broken (multi-sample) instrument: a compilation error.
	brokenModule := testModuleA()
	brokenModule.Instruments[0].Samples = append(brokenModule.Instruments[0].Samples, brokenModule.Instruments[0].Samples[0])
	if err := s.LoadModule(brokenModule, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected an error for multi-sample instrument")
	}

	// The stream must be in exactly the pre-failure state.
	if len(s.channels) != numChannelsBefore {
		t.Errorf("channels count changed after failed loads: got %d, want %d", len(s.channels), numChannelsBefore)
	}
	if s.bpm != bpmBefore {
		t.Errorf("bpm changed after failed loads: got %v, want %v", s.bpm, bpmBefore)
	}
	if info := s.GetInfo(); info != infoBefore {
		t.Errorf("GetInfo changed after failed loads: got %+v, want %+v", info, infoBefore)
	}
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != posBefore {
		t.Errorf("byte pos changed after failed loads: got %d, want %d", pos, posBefore)
	}

	// The old module must still be playable.
	n, err := s.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("old module is not playable after failed loads (n=%d, err=%v)", n, err)
	}

	// And a valid load must still succeed afterwards.
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module B after failures: %v", err)
	}
	assertCleanState(t, s, 8, 140)
	if n, nonzero := drainStream(t, s); n == 0 || !nonzero {
		t.Fatalf("module B is not playable after failed loads (n=%d, nonzero=%v)", n, nonzero)
	}
}

func TestStreamSyncEventTime(t *testing.T) {
	s := NewStream()

	var mu sync.Mutex
	var events []StreamEvent
	s.SetEventHandler(func(e StreamEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	getEvents := func() []StreamEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]StreamEvent(nil), events...)
	}

	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}

	// Loading a module reports a sync event with the new (zero) time.
	evs := getEvents()
	if len(evs) == 0 || evs[len(evs)-1].Kind != EventSync {
		t.Fatalf("no sync event after LoadModule")
	}
	if e := evs[len(evs)-1]; e.Time != 0 || e.SyncEventData() != 0 {
		t.Errorf("sync event after LoadModule: got time=%v data=%v, want 0/0", e.Time, e.SyncEventData())
	}

	if _, nonzero := drainStream(t, s); !nonzero {
		t.Fatalf("module A produced only silence")
	}
	if s.t == 0 {
		t.Fatalf("stream time did not advance")
	}

	// Rewind must report a sync event consistent with the new state,
	// not with the end-of-song time of the previous playback.
	s.Rewind()
	evs = getEvents()
	if e := evs[len(evs)-1]; e.Kind != EventSync || e.Time != 0 || e.SyncEventData() != 0 {
		t.Errorf("sync event after Rewind: got kind=%v time=%v data=%v, want sync/0/0",
			e.Kind, e.Time, e.SyncEventData())
	}

	// Note events after the rewind must use the new timeline as well.
	if _, err := s.Read(make([]byte, 8192)); err != nil {
		t.Fatalf("read after rewind: %v", err)
	}
	for _, e := range getEvents() {
		if e.Kind == EventNote {
			if e.Time != 0 {
				t.Errorf("first note event after rewind: got time %v, want 0", e.Time)
			}
			break
		}
	}
}

func TestStreamGetInfoAndPositionConsistency(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}

	wantBytesPerTick := bytesPerTickForBPM(120)
	if info := s.GetInfo(); info.BytesPerTick != uint(wantBytesPerTick) {
		t.Fatalf("BytesPerTick: got %d, want %d", info.BytesPerTick, wantBytesPerTick)
	}

	buf := make([]byte, 8192)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	pos, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != int64(n) {
		t.Errorf("byte pos: got %d, want %d (bytes read)", pos, n)
	}

	s.Rewind()
	pos, err = s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Errorf("byte pos after rewind: got %d, want 0", pos)
	}
	if info := s.GetInfo(); info.BytesPerTick != uint(wantBytesPerTick) {
		t.Errorf("BytesPerTick after rewind: got %d, want %d", info.BytesPerTick, wantBytesPerTick)
	}

	// Reloading with a different BPM must be reflected in GetInfo,
	// and the byte position must restart from zero.
	if err := s.LoadModule(testModuleB(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module B: %v", err)
	}
	wantBytesPerTick = bytesPerTickForBPM(140)
	if info := s.GetInfo(); info.BytesPerTick != uint(wantBytesPerTick) {
		t.Errorf("BytesPerTick after reload: got %d, want %d", info.BytesPerTick, wantBytesPerTick)
	}
	pos, err = s.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if pos != 0 {
		t.Errorf("byte pos after reload: got %d, want 0", pos)
	}
}

func TestSynthesizerAuditionsAreIsolated(t *testing.T) {
	m := testModule(1, 6, 120,
		[]xmfile.Instrument{
			testInstrument(64, 128, true),
			testInstrument(64, 128, false),
		},
		nil, [][]uint16{{0}})

	synth := NewSynthesizer(SynthesizerConfig{NumChannels: 1})
	if err := synth.LoadInstruments(m, LoadModuleConfig{}); err != nil {
		t.Fatalf("load instruments: %v", err)
	}

	// Audition the first instrument to its very end.
	if err := synth.PlayNote(0.05, xmfile.PatternNote{Note: 48, Instrument: 1}); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if n, nonzero := drainStream(t, synth); n == 0 || !nonzero {
		t.Fatalf("first audition produced no sound (n=%d, nonzero=%v)", n, nonzero)
	}

	// Sanity check: the first audition left some runtime state behind.
	ch := &synth.stream.channels[0]
	if ch.inst == nil || ch.volumeEnvelope.frame == 0 {
		t.Fatalf("first audition left no observable state")
	}

	// Audition the second instrument. PlayNote itself must reset
	// the previous audition state before the new note is played.
	if err := synth.PlayNote(0.05, xmfile.PatternNote{Note: 48, Instrument: 2}); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if ch.inst != nil {
		t.Errorf("previous instrument is still assigned after PlayNote")
	}
	if ch.volumeEnvelope.frame != 0 || ch.volumeEnvelope.value != 0 {
		t.Errorf("volume envelope was not reset: frame=%v value=%v",
			ch.volumeEnvelope.frame, ch.volumeEnvelope.value)
	}
	if ch.keyOn {
		t.Errorf("keyOn was not reset")
	}
	if synth.stream.bytePos != 0 || synth.stream.patternRowIndex != -1 {
		t.Errorf("stream position was not reset: bytePos=%d row=%d",
			synth.stream.bytePos, synth.stream.patternRowIndex)
	}

	// The second audition must actually produce sound; before the fix
	// the stream stayed at EOF after the first audition finished.
	n, nonzero := drainStream(t, synth)
	if n == 0 || !nonzero {
		t.Fatalf("second audition produced no sound (n=%d, nonzero=%v)", n, nonzero)
	}

	// And it must be the second instrument that is playing.
	if ch.inst != &synth.stream.module.instruments[1] {
		t.Errorf("second audition uses wrong instrument")
	}
}

func TestSynthesizerLoadFailureKeepsInstruments(t *testing.T) {
	m := testModule(1, 6, 120,
		[]xmfile.Instrument{testInstrument(64, 128, false)},
		nil, [][]uint16{{0}})

	synth := NewSynthesizer(SynthesizerConfig{NumChannels: 1})
	if err := synth.LoadInstruments(m, LoadModuleConfig{}); err != nil {
		t.Fatalf("load instruments: %v", err)
	}

	// Unsupported sample rate.
	if err := synth.LoadInstruments(m, LoadModuleConfig{SampleRate: 22050}); err == nil {
		t.Fatalf("expected an error for unsupported sample rate")
	}
	// Compilation error.
	broken := testModule(1, 6, 120,
		[]xmfile.Instrument{testInstrument(64, 128, false)},
		nil, [][]uint16{{0}})
	broken.Flags = 0
	if err := synth.LoadInstruments(broken, LoadModuleConfig{}); err == nil {
		t.Fatalf("expected an error for amiga frequency table")
	}

	// The previously loaded instruments must still be auditionable.
	if err := synth.PlayNote(0.05, xmfile.PatternNote{Note: 48, Instrument: 1}); err != nil {
		t.Fatalf("play note: %v", err)
	}
	if n, nonzero := drainStream(t, synth); n == 0 || !nonzero {
		t.Fatalf("instruments are not playable after failed loads (n=%d, nonzero=%v)", n, nonzero)
	}
}

func TestStreamConcurrentReadAndReload(t *testing.T) {
	s := NewStream()
	if err := s.LoadModule(testModuleA(), LoadModuleConfig{}); err != nil {
		t.Fatalf("load module A: %v", err)
	}
	s.SetLooping(true)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = s.Read(buf)
			_ = s.GetInfo()
			_, _ = s.Seek(0, io.SeekCurrent)
		}
	}()

	modA := testModuleA()
	modB := testModuleB()
	for i := 0; i < 60; i++ {
		switch i % 3 {
		case 0:
			s.Rewind()
		case 1:
			if err := s.LoadModule(modA, LoadModuleConfig{}); err != nil {
				t.Errorf("reload module A: %v", err)
			}
		case 2:
			if err := s.LoadModule(modB, LoadModuleConfig{}); err != nil {
				t.Errorf("reload module B: %v", err)
			}
		}
	}

	close(stop)
	wg.Wait()
}

func TestSynthesizerConcurrentPlayNoteAndRead(t *testing.T) {
	m := testModule(1, 6, 120,
		[]xmfile.Instrument{
			testInstrument(64, 128, false),
			testInstrument(64, 128, true),
		},
		nil, [][]uint16{{0}})

	synth := NewSynthesizer(SynthesizerConfig{NumChannels: 1})
	if err := synth.LoadInstruments(m, LoadModuleConfig{}); err != nil {
		t.Fatalf("load instruments: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = synth.Read(buf)
		}
	}()

	for i := 0; i < 60; i++ {
		inst := uint8(i%2) + 1
		if err := synth.PlayNote(0.02, xmfile.PatternNote{Note: 48, Instrument: inst}); err != nil {
			t.Errorf("play note: %v", err)
		}
		if i%5 == 0 {
			synth.Rewind()
		}
	}

	close(stop)
	wg.Wait()
}
