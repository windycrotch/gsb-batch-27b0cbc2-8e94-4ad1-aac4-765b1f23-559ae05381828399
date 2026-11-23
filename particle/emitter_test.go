package particle

import (
	"math"
	"testing"

	"github.com/hajimehoshi/ebiten/v2"
)

// newParticleTestTemplate returns a deterministic template
// (no random bits involved) bound to the given texture.
func newParticleTestTemplate(img *ebiten.Image, lifetime float64) *Template {
	tmpl := NewTemplate()
	tmpl.SetImage(img)
	tmpl.SetEmitInterval(0.05)
	tmpl.SetParticleLifetime(lifetime)
	tmpl.SetEmitBurst(1, 1)
	return tmpl
}

func TestEmitterParticlesLiveOutLifetimeAfterStop(t *testing.T) {
	img := ebiten.NewImage(2, 2)
	tmpl := newParticleTestTemplate(img, 0.25) // 250ms lifetime

	e := NewEmitter(tmpl)
	e.SetEmitting(true)
	for i := 0; i < 12; i++ {
		e.Update()
	}
	e.SetEmitting(false)

	if e.NumParticles() == 0 {
		t.Fatal("expected live particles after emitting")
	}

	// Right after stopping, the already-generated particles must
	// not disappear at once; every particle keeps its remaining lifetime.
	e.Update()
	if e.NumParticles() == 0 {
		t.Fatal("particles disappeared right after SetEmitting(false)")
	}
	for i := 0; i < 5; i++ {
		e.Update()
	}
	if e.NumParticles() == 0 {
		t.Fatal("particles disappeared before their lifetime expired")
	}

	// After the max remaining lifetime passes, all particles must be gone.
	for i := 0; i < 30; i++ {
		e.Update()
	}
	if e.NumParticles() != 0 {
		t.Fatalf("%d particles never expired", e.NumParticles())
	}
}

func TestEmitterParticleCounterOverflow(t *testing.T) {
	img := ebiten.NewImage(2, 2)
	// The max lifetime the uint16 millisecond counters can represent.
	tmpl := newParticleTestTemplate(img, 65.535)

	e := NewEmitter(tmpl)
	e.SetEmitting(true)
	e.Update()
	e.SetEmitting(false)

	if e.NumParticles() == 0 {
		t.Fatal("expected live particles")
	}

	// 200 updates * 0.5s = 100s of simulated time, way past the
	// ~65.5s lifetime. A uint16 counter overflow would wrap the
	// counter around and keep the particles alive (almost) forever.
	for i := 0; i < 200; i++ {
		e.UpdateWithDelta(0.5)
	}
	if e.NumParticles() != 0 {
		t.Fatalf("%d particles survived past their lifetime (counter overflow?)", e.NumParticles())
	}
}

func TestEmitterLifetimeMultiplierSaturates(t *testing.T) {
	img := ebiten.NewImage(2, 2)
	tmpl := newParticleTestTemplate(img, 3) // 3s base lifetime

	e := NewEmitter(tmpl)
	// 3s * 30 = 90s exceeds the max representable lifetime (~65.5s).
	// The lifetime must saturate instead of wrapping around, which
	// would make the particle die long before its time.
	e.SetLifetimeMultiplier(30)
	e.SetEmitting(true)
	e.Update()
	e.SetEmitting(false)

	if e.NumParticles() != 1 {
		t.Fatalf("expected 1 particle, got %d", e.NumParticles())
	}
	if e.particles[0].lifetime != math.MaxUint16 {
		t.Fatalf("lifetime = %d, want saturated %d", e.particles[0].lifetime, math.MaxUint16)
	}

	// 30 simulated seconds: the particle must still be alive
	// (a wrapped lifetime of 90000-65536=24464ms would kill it).
	for i := 0; i < 30; i++ {
		e.UpdateWithDelta(1)
	}
	if e.NumParticles() != 1 {
		t.Fatal("particle died before its (saturated) lifetime")
	}

	// Past the max lifetime, it must be gone.
	for i := 0; i < 40; i++ {
		e.UpdateWithDelta(1)
	}
	if e.NumParticles() != 0 {
		t.Fatal("particle outlived its lifetime")
	}
}

func TestEmitterRestartAfterStop(t *testing.T) {
	img := ebiten.NewImage(2, 2)
	tmpl := newParticleTestTemplate(img, 1)

	e := NewEmitter(tmpl)
	e.SetEmitting(true)
	for i := 0; i < 6; i++ {
		e.Update()
	}
	e.SetEmitting(false)
	for e.NumParticles() > 0 {
		e.Update()
	}
	// Restarting the emitter produces new particles again.
	e.SetEmitting(true)
	e.Update()
	if e.NumParticles() == 0 {
		t.Fatal("restarted emitter produced no particles")
	}
}
