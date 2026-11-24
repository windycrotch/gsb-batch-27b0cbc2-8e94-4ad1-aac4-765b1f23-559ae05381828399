package xm

import (
	"errors"

	"github.com/quasilyte/xm/xmfile"
)

// Synthesizer can be used to play individual XM notes.
//
// It is more efficient and convenient to use for this
// use case than a stream with a constant module re-loading.
//
// Experimental: synthesizer API may change in the near future.
type Synthesizer struct {
	stream       *Stream
	noteCompiler *moduleCompiler
}

type SynthesizerInstruments struct {
	instOnly    module
	numChannels int
}

// GetInstrumentMapping returns a mapping of an actual compiled
// instrument id to the index from the source XM module.
// They can be identical and the mapping would look like x=>x,
// but if some instruments were invalid or empty, the mapping
// will not be 1-to-1.
func (iset *SynthesizerInstruments) GetInstrumentMapping() []int {
	num := 0
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			num++
		}
	}
	m := make([]int, 0, num)
	for _, inst := range iset.instOnly.instruments {
		if inst.id != -1 {
			m = append(m, inst.id)
		}
	}
	return m
}

type SynthesizerConfig struct {
	NumChannels int
}

func NewSynthesizer(config SynthesizerConfig) *Synthesizer {
	stream := NewStream()
	stream.channels = make([]streamChannel, config.NumChannels)
	stream.activeChannels = make([]*streamChannel, config.NumChannels)
	return &Synthesizer{
		stream:       stream,
		noteCompiler: newSynthCompiler(),
	}
}

// SetVolume adjusts the global volume scaling for the underlying stream.
func (s *Synthesizer) SetVolume(v float64) {
	s.stream.SetVolume(v)
}

// SetInstruments loads the instruments from a list received from GetInstruments.
func (s *Synthesizer) SetInstruments(is *SynthesizerInstruments) error {
	if len(s.stream.channels) != is.numChannels {
		return errors.New("mismatching channel count")
	}

	s.stream.mu.Lock()
	s.stream.activeChannels = s.stream.activeChannels[:0]
	s.stream.assignCompiledModule(is.instOnly)
	s.stream.mu.Unlock()

	return nil
}

// GetInstruments returns the previously loaded instruments.
// It is intended to be used in combination with SetInstruments
// to avoid the need for repeated LoadInstruments for the
// same module. It is much more efficient as it avoids
// the module re-compilation.
// The instruments are only compatible if the number of channels is matching.
func (s *Synthesizer) GetInstruments() *SynthesizerInstruments {
	s.stream.mu.Lock()
	defer s.stream.mu.Unlock()

	return &SynthesizerInstruments{
		instOnly:    s.stream.module,
		numChannels: len(s.stream.channels),
	}
}

// LoadInstruments prepares the instruments from the module
// for further use.
//
// Loading instruments involves module compilation,
// so it should not be called on a hot path repeatedly.
// If you need multiple modules at once, consider
// using several synthesizers, one per module.
//
// The patterns don't really matter as this method
// is only interested in instruments (and samples).
func (s *Synthesizer) LoadInstruments(m *xmfile.Module, config LoadModuleConfig) error {
	s.stream.applyConfigDefaults(m, &config)

	if config.SampleRate != 44100 {
		return errors.New("unsupported sample rate (only 44100 is supported)")
	}

	// Compile the instruments before touching the stream state.
	// If the compilation fails, the previously loaded instruments
	// (if any) remain usable.
	instOnly, err := s.compileModuleInstruments(m, config)
	if err != nil {
		return err
	}

	s.stream.mu.Lock()
	s.stream.activeChannels = s.stream.activeChannels[:0]
	s.stream.assignCompiledModule(instOnly)
	s.stream.mu.Unlock()

	return nil
}

func (s *Synthesizer) compileModuleInstruments(m *xmfile.Module, config LoadModuleConfig) (module, error) {
	instOnly := xmfile.Module{
		Name:           m.Name,
		Version:        m.Version,
		NumInstruments: m.NumInstruments,
		Flags:          m.Flags,
		Instruments:    m.Instruments,
	}

	compiled, err := compileModule(&instOnly, moduleConfig{
		sampleRate: config.SampleRate,
		bpm:        config.BPM,
		tempo:      config.Tempo,
		subSamples: config.LinearInterpolation,
	})
	if err != nil {
		return compiled, err
	}

	compiled.patterns = []pattern{
		{
			numChannels: len(s.stream.channels),
			numRows:     1,
			notes:       make([]uint16, len(s.stream.channels)),
		},
	}
	compiled.patternOrder = []*pattern{
		&compiled.patterns[0],
	}

	pat := &compiled.patterns[0]
	for i := range pat.notes {
		pat.notes[i] = uint16(i)
	}

	return compiled, nil
}

// prepareToPlay resets the note/effect compiler tables and puts
// the stream back to the beginning of the (single-row) synth pattern.
// Every audition starts from a clean slate: the previous note's
// envelopes, slides, vibrato and other per-channel effect states
// are all reset, so consecutive PlayNote calls never contaminate
// each other.
// The caller should hold s.stream.mu.
func (s *Synthesizer) prepareToPlay(duration float64) {
	s.noteCompiler.reset(&s.stream.module)

	s.stream.module.effectTab = s.stream.module.effectTab[:0]
	s.stream.module.noteTab = s.stream.module.noteTab[:0]

	s.stream.rewind()

	if duration == 0 {
		s.stream.setTempo(240)
	} else {
		s.stream.setTempo(1 + int(ticksPerSecond(s.stream.bpm)*duration))
	}
}

// PlayNote plays one or more notes up to the specified duration.
// Using 0 for the duration will play it for several seconds.
func (s *Synthesizer) PlayNote(duration float64, notes ...xmfile.PatternNote) error {
	s.stream.mu.Lock()
	defer s.stream.mu.Unlock()

	s.prepareToPlay(duration)

	chansUsed := 0
	for _, note := range notes {
		n, _, err := s.noteCompiler.compileNote(note)
		if err != nil {
			return err
		}
		s.stream.module.noteTab = append(s.stream.module.noteTab, n)
		chansUsed++
		if chansUsed >= len(s.stream.channels) {
			break
		}
	}

	for chansUsed < len(s.stream.channels) {
		s.stream.module.noteTab = append(s.stream.module.noteTab, patternNote{})
		chansUsed++
	}

	return nil
}

func (s *Synthesizer) Read(b []byte) (int, error) {
	return s.stream.Read(b)
}

func (s *Synthesizer) Rewind() {
	s.stream.Rewind()
}

func (s *Synthesizer) Seek(offset int64, whence int) (int64, error) {
	return s.stream.Seek(offset, whence)
}
