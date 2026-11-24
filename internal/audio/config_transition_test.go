package audio

// config_transition_test.go is the acceptance suite for the atomic runtime
// config transition and the converged stop/restart lifecycle:
//
//   - a tier/toggle change is applied by the DSP worker as ONE atomic transition
//     before the next frame is processed, with the gate latch, noise-floor
//     estimate, and RNNoise speech-probability stream re-initialized or smoothly
//     migrated (no old-config residue);
//   - config switching is safe concurrently with engine stop, and repeated
//     start/stop cycles leave no cross-frame state behind;
//   - when the native denoiser is unavailable (Passthrough fallback) the advanced
//     VAD automatically drops back to the energy gate instead of muting the mic;
//   - the real-time callback never blocks and emits neither a multi-frame
//     silence nor a click while the switching happens.

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jodagreyhame/SoundBoard/internal/apm"
	"github.com/jodagreyhame/SoundBoard/internal/denoise"
)

// recordingAPM is a test apm.Processor that records Reconfigure calls and the
// order of Reconfigure vs ProcessCapture, so a test can prove the new config is
// applied BEFORE the next frame is processed.
type recordingAPM struct {
	reconfigs              int
	reconfigsAtLastProcess int
	processed              int
	lastCfg                apm.Config
}

func (r *recordingAPM) ProcessCapture(frame []float32) int {
	r.processed++
	r.reconfigsAtLastProcess = r.reconfigs
	return 0
}
func (r *recordingAPM) Reconfigure(cfg apm.Config) int {
	r.reconfigs++
	r.lastCfg = cfg
	return 0
}
func (r *recordingAPM) Close() {}

// closableFakeDenoiser is a test Denoiser that tracks Close calls so a test can
// prove the old native state is released when the worker rebuilds the denoiser
// on a chain-config transition. It never modifies the frame.
type closableFakeDenoiser struct {
	prob   float32
	calls  int
	closed bool
}

func (f *closableFakeDenoiser) Process(frame []float32) float32 {
	f.calls++
	return f.prob
}
func (f *closableFakeDenoiser) Close() { f.closed = true }

// swapInRecordingAPM replaces a worker's APM with a recording fake, closing the
// real one so no native state leaks.
func swapInRecordingAPM(w *micWorker) *recordingAPM {
	w.apm.Close()
	rec := &recordingAPM{}
	w.apm = rec
	return rec
}

// TestConfigTransitionAppliesBeforeNextFrame is the core atomicity test: several
// toggles flipped between two frames must coalesce into ONE observable
// transition, the APM must be reconfigured BEFORE the next ProcessCapture, and
// the applied snapshot must track every flipped control.
func TestConfigTransitionAppliesBeforeNextFrame(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetEchoCancellation(false)
	e.SetAdvancedVoiceActivity(false)
	w := newWorkerFor(e)
	rec := swapInRecordingAPM(w)

	// Flip a chain control AND a gate control in the same inter-frame gap.
	e.SetNoiseSuppressionTier("high")
	e.SetAGC(true)
	e.SetEchoCancellation(true)
	e.SetAdvancedVoiceActivity(true)

	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)

	if rec.reconfigs != 1 {
		t.Fatalf("expected exactly one APM Reconfigure for the coalesced transition, got %d", rec.reconfigs)
	}
	if rec.processed != 1 || rec.reconfigsAtLastProcess != 1 {
		t.Fatalf("Reconfigure must run BEFORE the frame's ProcessCapture (reconfigs=%d at process #%d)",
			rec.reconfigsAtLastProcess, rec.processed)
	}
	if !rec.lastCfg.NoiseSuppressionEnabled || rec.lastCfg.NoiseSuppressionLevel != apm.NSLevelHigh {
		t.Fatalf("new NS config not applied: %+v", rec.lastCfg)
	}
	if !rec.lastCfg.GainControlEnabled || !rec.lastCfg.EchoCancellationEnabled {
		t.Fatalf("new AGC/echo config not applied: %+v", rec.lastCfg)
	}
	if w.transitions != 1 {
		t.Fatalf("multiple toggles in one gap must coalesce into ONE transition, got %d", w.transitions)
	}
	if w.nsTierApplied != nsTierHigh || !w.agcApplied || !w.aecApplied || !w.advVADApplied {
		t.Fatalf("applied snapshot must track every flipped control: tier=%d agc=%v aec=%v advVAD=%v",
			w.nsTierApplied, w.agcApplied, w.aecApplied, w.advVADApplied)
	}

	// A steady-state frame with no further change must NOT reconfigure again.
	frame2, _ := sineFrame(220, 0.2, 0)
	w.process(frame2)
	if rec.reconfigs != 1 || w.transitions != 1 {
		t.Fatalf("no config change -> no further transition, got reconfigs=%d transitions=%d", rec.reconfigs, w.transitions)
	}
}

// TestConfigTransitionResetsGateLatchOnVADSourceSwitch proves the gate latch
// (including its hold window) does not leak across a VAD-method switch: the
// advanced-VAD hold is 25 frames, the energy hold only 3, so a stale latch would
// keep the gate open ~250ms into the new config. The latch must be re-decided on
// the transition frame while the gain RAMP migrates smoothly (click-free).
func TestConfigTransitionResetsGateLatchOnVADSourceSwitch(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetGateSensitivity(0.15)

	fake := &fakeDenoiser{prob: 0.95}
	w := newWorkerWithDen(e, fake)
	defer w.apm.Close()

	// Open the gate via high speech probability on silent frames.
	for i := 0; i < 40; i++ {
		w.process(make([]float32, dspFrame))
	}
	if !w.gate.open || w.gate.holdLeft == 0 {
		t.Fatalf("setup: latch should be open with a live VAD hold, got open=%v hold=%d", w.gate.open, w.gate.holdLeft)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("setup: gate should be fully open, GateLevel = %v", gl)
	}

	// Switch the VAD method: advanced VAD OFF, frames still silent. The energy
	// latch would decide CLOSED on this frame; the stale 25-frame VAD hold must
	// NOT carry over.
	e.SetAdvancedVoiceActivity(false)
	w.process(make([]float32, dspFrame))

	if w.gate.holdLeft != 0 {
		t.Fatalf("stale VAD hold survived the transition: holdLeft = %d", w.gate.holdLeft)
	}
	if w.gate.open {
		t.Fatal("latch must be re-decided under the new config on the transition frame")
	}
	// Smooth migration: the gain ramp continues from its current position (~1)
	// rather than hard-muting, so the transition is click-free.
	if w.gate.gain < 0.5 {
		t.Fatalf("gain ramp should migrate smoothly (no hard mute), gain = %v", w.gate.gain)
	}

	// With the stale hold cleared, the gate reaches silence within the energy
	// hold + release ramp — far sooner than the old 25-frame VAD hold would have
	// allowed.
	for i := 0; i < 60; i++ {
		w.process(make([]float32, dspFrame))
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("gate should be fully closed well under the old VAD hold, GateLevel = %v", gl)
	}
}

// TestConfigTransitionResetsNoiseFloor proves the automatic-input-sensitivity
// noise-floor estimate is re-seeded when the chain config or the auto-sensitivity
// toggle changes: the estimate is expressed in the old config's level units and
// must not jam the new config's gate.
func TestConfigTransitionResetsNoiseFloor(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAutoSensitivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	w := newWorkerFor(e)
	defer w.apm.Close()

	// Seed a HIGH noise floor with loud frames.
	ph := 0.0
	for i := 0; i < 50; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.25, ph)
		w.process(frame)
	}
	if !w.floorSeeded || w.noiseFloor < 0.05 {
		t.Fatalf("setup: floor should be seeded high, got seeded=%v floor=%v", w.floorSeeded, w.noiseFloor)
	}

	// Toggle AGC: a chain transition. The next (quiet) frame must RE-SEED the
	// floor from the new config's signal, not decay slowly from the old value.
	e.SetAGC(true)
	quiet, ph2 := sineFrame(300, 0.003, ph)
	w.process(quiet)
	if !w.floorSeeded {
		t.Fatal("floor should re-seed from the first frame under the new config")
	}
	if w.noiseFloor > 0.01 {
		t.Fatalf("floor must re-seed from the new config's first frame (~0.002), got %v (old-config residue)", w.noiseFloor)
	}

	// Raise the floor again (the follower rises slowly by design, so a run of loud
	// frames lifts it well clear of the quiet re-seed), then toggle
	// auto-sensitivity itself off: the tracked floor belongs to the automatic
	// regime and must be dropped.
	for i := 0; i < 50; i++ {
		var frame []float32
		frame, ph2 = sineFrame(300, 0.25, ph2)
		w.process(frame)
	}
	if w.noiseFloor < 0.01 {
		t.Fatalf("setup: floor should have tracked up from the quiet re-seed, got %v", w.noiseFloor)
	}
	e.SetAutoSensitivity(false)
	frame, _ := sineFrame(300, 0.003, ph2)
	w.process(frame)
	if w.floorSeeded || w.noiseFloor != 0 {
		t.Fatalf("toggling auto-sensitivity off must drop the tracked floor, got seeded=%v floor=%v", w.floorSeeded, w.noiseFloor)
	}
}

// TestConfigTransitionRebuildsDenoiser proves a chain-config transition rebuilds
// the RNNoise denoiser — closing the old native state and restarting the
// speech-probability stream from clean initial values — and that denVAD is
// recomputed from the NEW denoiser.
func TestConfigTransitionRebuildsDenoiser(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("none")
	w := newWorkerFor(e)
	defer w.apm.Close()

	first := &closableFakeDenoiser{prob: 0.9}
	var built []*closableFakeDenoiser
	w.newDen = func(suppress bool) denoise.Denoiser {
		d := &closableFakeDenoiser{prob: 0.8}
		built = append(built, d)
		return d
	}
	w.den = first
	w.denVAD = true

	e.SetNoiseSuppressionTier("strong")
	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)

	if len(built) != 1 {
		t.Fatalf("chain transition must rebuild the denoiser exactly once, got %d builds", len(built))
	}
	if !first.closed {
		t.Fatal("the old denoiser must be Closed on rebuild (native resource release)")
	}
	if w.den != built[0] {
		t.Fatal("worker must use the freshly built denoiser after the transition")
	}
	if !w.denVAD {
		t.Fatal("a VAD-capable rebuild must keep denVAD true")
	}
	if built[0].calls == 0 {
		t.Fatal("Strong tier must run the rebuilt denoiser on the transition frame itself")
	}
}

// TestNativeUnavailableAdvancedVADFallsBackToEnergy is the native-unavailable
// fallback guarantee: with the denoiser in Passthrough (no cgo / native state
// allocation failed), the advanced VAD must automatically drop to the energy
// gate so the mic is NOT permanently muted.
func TestNativeUnavailableAdvancedVADFallsBackToEnergy(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true) // user wants probability VAD...
	e.SetNoiseSuppressionTier("none")
	e.SetGateSensitivity(0.15)
	w := newWorkerFor(e)
	defer w.apm.Close()

	// ...but the native denoiser is unavailable: Passthrough returns p==0, which
	// would hold the probability latch shut forever. The production derivation
	// must mark it not-VAD-capable so the energy gate takes over.
	w.den = denoise.Passthrough{}
	w.denVAD = denoiserVADCapable(w.den)
	if w.denVAD {
		t.Fatal("Passthrough must be derived as not VAD-capable")
	}

	ph := 0.0
	for i := 0; i < 50; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("advanced VAD must fall back to the energy gate on Passthrough (mic must not stay muted), GateLevel = %v", gl)
	}
}

// TestRuntimeDenoiseFailureDropsAdvancedVADToEnergy covers the RUNTIME fallback:
// the denoiser was VAD-capable at start, but a chain transition rebuilds it and
// the native state can no longer be allocated (Passthrough). The advanced VAD
// must drop to the energy gate from that frame on — never a permanently muted mic.
func TestRuntimeDenoiseFailureDropsAdvancedVADToEnergy(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetGateSensitivity(0.15)
	w := newWorkerFor(e)
	defer w.apm.Close()

	// Factory that succeeds once, then simulates a native allocation failure.
	builds := 0
	w.newDen = func(suppress bool) denoise.Denoiser {
		builds++
		if builds == 1 {
			return &closableFakeDenoiser{prob: 0.95}
		}
		return denoise.Passthrough{}
	}
	w.den = w.newDen(true)
	w.denVAD = denoiserVADCapable(w.den)
	if !w.denVAD {
		t.Fatal("setup: initial denoiser should be VAD-capable")
	}

	// A chain toggle triggers the rebuild; the new denoiser is Passthrough.
	e.SetNoiseSuppressionTier("high")
	frame, ph := loudMonoFrame(0)
	w.process(frame)
	if w.denVAD {
		t.Fatal("denVAD must be recomputed on rebuild: Passthrough fallback must disable probability VAD")
	}

	// The energy gate takes over immediately: loud frames still open the gate.
	for i := 0; i < 50; i++ {
		var f []float32
		f, ph = loudMonoFrame(ph)
		w.process(f)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("after a runtime denoise failure the energy gate must take over, GateLevel = %v", gl)
	}
}

// TestConfigSwitchConcurrentWithStop hammers every runtime toggle from many
// goroutines while the engine stops the worker, proving the switch and stop
// paths converge without deadlock or data race (run with -race), and that the
// engine still works when started again afterwards.
func TestConfigSwitchConcurrentWithStop(t *testing.T) {
	e := NewEngine(nil, nil)
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.SetMicMode("vad")
	e.denFactory = func(bool) denoise.Denoiser { return &closableFakeDenoiser{prob: 0.9} }

	e.startWorker()
	w := e.worker

	quit := make(chan struct{})
	var wg sync.WaitGroup

	// Producer: keep the input ring fed so the worker keeps processing frames.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ph := 0.0
		for {
			select {
			case <-quit:
				return
			default:
			}
			var frame []float32
			frame, ph = sineFrame(220, 0.3, ph)
			e.inRing.push(frame)
			time.Sleep(time.Millisecond)
		}
	}()

	// Switchers: hammer every toggle concurrently, as the UI/hotkeys would.
	tiers := []string{"none", "standard", "high", "strong"}
	modes := []string{"vad", "always", "ptt", "mute"}
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			e.SetNoiseSuppressionTier(tiers[i%len(tiers)])
			e.SetAGC(i%2 == 0)
			e.SetEchoCancellation(i%3 == 0)
			time.Sleep(300 * time.Microsecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			e.SetAdvancedVoiceActivity(i%2 == 0)
			e.SetAutoSensitivity(i%3 == 0)
			e.SetGateSensitivity(float32(i%10) / 10)
			time.Sleep(250 * time.Microsecond)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			e.SetMicMode(modes[i%len(modes)])
			e.SetPTTDown(i%2 == 0)
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// Let the switches overlap the running worker, then stop WHILE switching.
	time.Sleep(100 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		e.stopWorker()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopWorker blocked: the worker must exit and release its native resources")
	}
	close(quit)
	wg.Wait()

	if e.worker != nil {
		t.Fatal("stopWorker must clear the worker handle")
	}
	if w.transitions == 0 {
		t.Fatal("the hammered toggles should have produced observable transitions on the worker")
	}

	// The engine must still function after the concurrent stop: restart the
	// worker and confirm a clean round-trip.
	e.inRing.reset()
	e.outRing.reset()
	e.SetMicMode("always")
	e.startWorker()
	defer e.stopWorker()
	frame, _ := sineFrame(220, 0.3, 0)
	e.inRing.push(frame)
	out := make([]float32, dspFrame)
	deadline := time.Now().Add(2 * time.Second)
	for e.outRing.length() < dspFrame && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := e.outRing.pull(out); n == 0 {
		t.Fatal("restarted worker produced no output")
	}
}

// TestRepeatedStartStopLeavesNoResidualState runs the real start/stop lifecycle
// (with the REAL denoise.New factory, so native RNNoise/APM state is genuinely
// created and released each cycle) and proves every restart begins from clean
// initial cross-frame state: closed gate, unseeded noise floor, zeroed gate
// level, empty rings.
func TestRepeatedStartStopLeavesNoResidualState(t *testing.T) {
	e := NewEngine(nil, nil)
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.SetMicMode("vad")
	e.SetAutoSensitivity(true)
	e.SetNoiseSuppressionTier("high")
	e.SetGateSensitivity(0.15)

	var prev *micWorker
	for cycle := 0; cycle < 3; cycle++ {
		e.startWorker()
		w := e.worker
		if w == nil {
			t.Fatal("startWorker must install a worker")
		}
		if w == prev {
			t.Fatal("each start must build a NEW worker")
		}
		// The rings are empty (Stop reset them) and no producer is running, so the
		// worker is idle in its nap loop and cannot be writing these fields: every
		// piece of cross-frame state must be at its clean initial value.
		if w.gate.gain != 0 || w.gate.open || w.gate.holdLeft != 0 {
			t.Fatalf("cycle %d: gate must start closed, got gain=%v open=%v hold=%d", cycle, w.gate.gain, w.gate.open, w.gate.holdLeft)
		}
		if w.floorSeeded || w.noiseFloor != 0 {
			t.Fatalf("cycle %d: noise floor must start unseeded, got seeded=%v floor=%v", cycle, w.floorSeeded, w.noiseFloor)
		}
		if w.transitions != 0 {
			t.Fatalf("cycle %d: no transitions may be recorded on a fresh worker, got %d", cycle, w.transitions)
		}
		if w.apm == nil || w.den == nil {
			t.Fatalf("cycle %d: fresh worker must hold fresh native handles", cycle)
		}

		// Drive loud frames through the rings until the gate opens and the floor
		// seeds — real cross-frame state accumulated under this cycle's config.
		ph := 0.0
		deadline := time.Now().Add(2 * time.Second)
		for e.GateLevel() < 0.5 && time.Now().Before(deadline) {
			var frame []float32
			frame, ph = sineFrame(220, 0.3, ph)
			e.inRing.push(frame)
			time.Sleep(time.Millisecond)
		}
		if e.GateLevel() < 0.5 {
			t.Fatalf("cycle %d: gate should have opened on loud frames", cycle)
		}

		// The production stop path: worker exits, native resources are released,
		// rings reset, gate level zeroed.
		if err := e.Stop(); err != nil {
			t.Fatalf("cycle %d: Stop: %v", cycle, err)
		}
		if e.worker != nil {
			t.Fatalf("cycle %d: Stop must clear the worker", cycle)
		}
		if gl := e.GateLevel(); gl != 0 {
			t.Fatalf("cycle %d: Stop must zero the published gate level, got %v", cycle, gl)
		}
		if e.inRing.length() != 0 || e.outRing.length() != 0 {
			t.Fatalf("cycle %d: Stop must leave both rings empty, got %d/%d", cycle, e.inRing.length(), e.outRing.length())
		}
		prev = w
	}
}

// TestRTCallbackUnblockedDuringConfigSwitch drives the REAL duplex callback
// while every DSP toggle is hammered from another goroutine, proving the audio
// thread never blocks (all periods complete well within a deadline) and the
// cable output never goes silent for more than a frame's worth of periods
// (one frame = 10ms ≈ 2.5 periods) while the switching is in flight.
func TestRTCallbackUnblockedDuringConfigSwitch(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("always")
	// Deterministic denoiser through the real lifecycle seam: the Strong tier then
	// leaves the (loud) signal audible, so any silence can only come from a
	// transition glitch, never from the network suppressing a synthetic tone.
	e.denFactory = func(bool) denoise.Denoiser { return &closableFakeDenoiser{prob: 0.9} }
	e.primeOutputRing()
	e.startWorker()
	defer e.stopWorker()

	// Hammer the chain toggles (tiers that keep the test tone audible: none and
	// strong) plus AGC/echo/advanced-VAD/auto-sensitivity from a second
	// goroutine, as the UI would mid-call.
	quit := make(chan struct{})
	var switcher sync.WaitGroup
	switcher.Add(1)
	go func() {
		defer switcher.Done()
		tiers := []string{"none", "strong"}
		for i := 0; ; i++ {
			select {
			case <-quit:
				return
			default:
			}
			e.SetNoiseSuppressionTier(tiers[i%2])
			e.SetAGC(i%2 == 0)
			e.SetEchoCancellation(i%3 == 0)
			e.SetAdvancedVoiceActivity(i%2 == 0)
			e.SetAutoSensitivity(i%3 == 0)
			time.Sleep(200 * time.Microsecond)
		}
	}()
	defer func() {
		close(quit)
		switcher.Wait()
	}()

	// Drive the callback at the real-time cadence from a goroutine; the test
	// fails on timeout if any callback invocation ever blocks.
	const periodsToRun = 400
	done := make(chan struct{})
	silentRun := make(chan int, 1)
	go func() {
		defer close(done)
		micF := make([]float32, periodFrames*channels)
		outB := make([]byte, periodFrames*channels*4)
		inc := 2 * 3.141592653589793 * 220.0 / fs
		phase := 0.0
		worst := 0
		run := 0
		for p := 0; p < periodsToRun; p++ {
			for f := 0; f < periodFrames; f++ {
				v := float32(0.3 * math.Sin(phase))
				phase += inc
				micF[f*channels] = v
				micF[f*channels+1] = v
			}
			e.duplexCallback(outB, f32bytes(micF), periodFrames)
			if p >= 10 { // skip the primed-silence warm-up
				var peak float32
				for _, s := range bytesAsF32(outB) {
					if s < 0 {
						s = -s
					}
					if s > peak {
						peak = s
					}
				}
				if peak < 1e-4 {
					run++
					if run > worst {
						worst = run
					}
				} else {
					run = 0
				}
			}
			time.Sleep(150 * time.Microsecond)
		}
		silentRun <- worst
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("duplex callback blocked during concurrent config switching")
	}
	if worst := <-silentRun; worst > 2 {
		t.Fatalf("output went silent for %d consecutive periods (> one 10ms frame) during config switching", worst)
	}
}
