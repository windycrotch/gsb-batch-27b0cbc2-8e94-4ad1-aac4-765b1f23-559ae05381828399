package particle

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/hajimehoshi/ebiten/v2"
)

func activeBucketCount(r *Renderer) int {
	n := 0
	for _, b := range r.bucketList {
		if b.img != nil {
			n++
		}
	}
	return n
}

func rendererEmitterCount(r *Renderer) int {
	n := 0
	for _, b := range r.bucketList {
		if b.img == nil {
			continue
		}
		n += len(b.emitters)
	}
	return n
}

func rendererParticleCount(r *Renderer) int {
	n := 0
	for _, b := range r.bucketList {
		if b.img == nil {
			continue
		}
		for _, e := range b.emitters {
			n += e.NumParticles()
		}
	}
	return n
}

// rendererConsistencyProblems validates the invariants that must hold
// between the bucket list, the free list and the texture→bucket map:
//   - bucketList[i].id == i
//   - a bucket is retired (img == nil) iff it is in the free list (exactly once)
//   - no two active buckets share the same texture
//   - the map points exactly at the active buckets, and every active
//     bucket is reachable through the map
func rendererConsistencyProblems(r *Renderer) []string {
	var problems []string

	inFreeList := make(map[*rendererBucket]bool, len(r.freeList))
	for _, b := range r.freeList {
		if b == nil {
			problems = append(problems, "nil bucket in the free list")
			continue
		}
		if inFreeList[b] {
			problems = append(problems, fmt.Sprintf("bucket %d appears in the free list twice", b.id))
		}
		inFreeList[b] = true
		if b.img != nil {
			problems = append(problems, fmt.Sprintf("free-list bucket %d still has a texture", b.id))
		}
	}

	bucketByImg := make(map[*ebiten.Image]*rendererBucket)
	for i, b := range r.bucketList {
		if b.id != i {
			problems = append(problems, fmt.Sprintf("bucketList[%d].id = %d", i, b.id))
		}
		if b.img == nil {
			if !inFreeList[b] {
				problems = append(problems, fmt.Sprintf("retired bucket %d is not in the free list", b.id))
			}
			continue
		}
		if inFreeList[b] {
			problems = append(problems, fmt.Sprintf("active bucket %d is in the free list", b.id))
		}
		if prev, ok := bucketByImg[b.img]; ok {
			problems = append(problems, fmt.Sprintf("buckets %d and %d share the same texture", prev.id, b.id))
		}
		bucketByImg[b.img] = b
		id, ok := r.bucketIDByImage[b.img]
		if !ok {
			problems = append(problems, fmt.Sprintf("active bucket %d has no texture->bucket mapping", b.id))
		} else if id != b.id {
			problems = append(problems, fmt.Sprintf("texture->bucket mapping = %d, want %d", id, b.id))
		}
	}

	if len(r.bucketIDByImage) != len(bucketByImg) {
		problems = append(problems,
			fmt.Sprintf("mapping has %d entries, but there are %d active buckets",
				len(r.bucketIDByImage), len(bucketByImg)))
	}
	for img, id := range r.bucketIDByImage {
		if id < 0 || id >= len(r.bucketList) {
			problems = append(problems, fmt.Sprintf("mapping points to invalid bucket id %d", id))
			continue
		}
		if r.bucketList[id].img != img {
			problems = append(problems,
				fmt.Sprintf("mapping points to bucket %d that is retired or bound to another texture", id))
		}
	}

	return problems
}

func checkRendererConsistency(t *testing.T, r *Renderer) {
	t.Helper()
	for _, p := range rendererConsistencyProblems(r) {
		t.Error(p)
	}
}

// bucketContentProblems reports emitters that must have been removed
// from the renderer's visible set during the last Draw.
func bucketContentProblems(r *Renderer) []string {
	var problems []string
	for _, b := range r.bucketList {
		if b.img == nil {
			continue
		}
		for _, e := range b.emitters {
			if e.IsDisposed() {
				problems = append(problems, "disposed emitter left in a bucket")
			}
			if !e.emitting && e.NumParticles() == 0 {
				problems = append(problems, "stopped and drained emitter left in a bucket")
			}
		}
	}
	return problems
}

func TestRendererStoppedEmitterRemovedAfterParticlesExpire(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	img := ebiten.NewImage(2, 2)
	tmpl := newParticleTestTemplate(img, 0.25)

	r := NewRenderer()
	e := NewEmitter(tmpl)
	e.SetEmitting(true)
	r.AddEmitter(e)
	for i := 0; i < 12; i++ {
		e.Update()
	}
	if e.NumParticles() == 0 {
		t.Fatal("expected live particles")
	}
	e.SetEmitting(false)

	// The emitter stopped, but it still has live particles:
	// it must stay in the visible set and keep being drawn.
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererEmitterCount(r) != 1 {
		t.Fatal("emitter with live particles was removed from the visible set too early")
	}
	if activeBucketCount(r) != 1 {
		t.Fatalf("active buckets = %d, want 1", activeBucketCount(r))
	}
	if rendererParticleCount(r) == 0 {
		t.Fatal("live particles of a stopped emitter are not visible")
	}

	// Let all particles expire; the emitter must then leave the
	// visible set and its bucket must be recycled.
	for e.NumParticles() > 0 {
		e.Update()
	}
	r.Draw(dst)
	checkRendererConsistency(t, r)
	for _, p := range bucketContentProblems(r) {
		t.Error(p)
	}
	if rendererEmitterCount(r) != 0 {
		t.Fatal("drained emitter was not removed from the visible set")
	}
	if activeBucketCount(r) != 0 {
		t.Fatalf("bucket without live emitters was not recycled, active buckets = %d", activeBucketCount(r))
	}
	if len(r.bucketIDByImage) != 0 {
		t.Fatal("stale texture->bucket mapping after bucket recycling")
	}
	if len(r.freeList) != 1 {
		t.Fatalf("free list size = %d, want 1 (recycled bucket)", len(r.freeList))
	}
}

func TestRendererBucketSurvivesUntilAllEmittersDrained(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	img := ebiten.NewImage(2, 2)
	// Two emitters sharing one texture (one bucket), with different lifetimes.
	e1 := NewEmitter(newParticleTestTemplate(img, 0.1))
	e2 := NewEmitter(newParticleTestTemplate(img, 1.0))

	r := NewRenderer()
	e1.SetEmitting(true)
	e2.SetEmitting(true)
	r.AddEmitter(e1)
	r.AddEmitter(e2)
	if len(r.bucketList) != 1 {
		t.Fatalf("buckets = %d, want 1 for a shared texture", len(r.bucketList))
	}
	for i := 0; i < 6; i++ {
		e1.Update()
		e2.Update()
	}
	e1.SetEmitting(false)
	e2.SetEmitting(false)

	// Drain e1 (short lifetime) while e2 still has live particles.
	for e1.NumParticles() > 0 {
		e1.Update()
		e2.Update()
	}
	if e2.NumParticles() == 0 {
		t.Fatal("test assumption broken: e2 drained too early")
	}
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererEmitterCount(r) != 1 {
		t.Fatalf("emitters in visible set = %d, want 1 (only drained e1 removed)", rendererEmitterCount(r))
	}
	if activeBucketCount(r) != 1 {
		t.Fatal("bucket recycled while it still has a live emitter")
	}
	if len(r.bucketIDByImage) != 1 {
		t.Fatal("texture->bucket mapping lost while bucket is still active")
	}

	// Drain e2 as well; only now the bucket may be recycled.
	for e2.NumParticles() > 0 {
		e2.Update()
	}
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererEmitterCount(r) != 0 || activeBucketCount(r) != 0 {
		t.Fatal("bucket was not recycled after all of its emitters drained")
	}
	if len(r.bucketIDByImage) != 0 {
		t.Fatal("stale texture->bucket mapping after bucket recycling")
	}
}

func TestRendererBucketRecycleAndReuse(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	imgA := ebiten.NewImage(2, 2)
	imgB := ebiten.NewImage(2, 2)

	r := NewRenderer()
	e1 := NewEmitter(newParticleTestTemplate(imgA, 1))
	e1.SetEmitting(true)
	r.AddEmitter(e1)
	e1.Update()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if activeBucketCount(r) != 1 || len(r.bucketIDByImage) != 1 {
		t.Fatal("expected 1 active bucket with 1 mapping")
	}

	// Disposing the only emitter retires its bucket on the next Draw.
	e1.Dispose()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	for _, p := range bucketContentProblems(r) {
		t.Error(p)
	}
	if activeBucketCount(r) != 0 {
		t.Fatal("bucket with only disposed emitters was not recycled")
	}
	if len(r.bucketIDByImage) != 0 {
		t.Fatal("texture->bucket mapping of a recycled bucket was not removed")
	}
	if len(r.freeList) != 1 {
		t.Fatalf("free list size = %d, want 1", len(r.freeList))
	}

	// A new emitter with a different texture reuses the retired bucket;
	// the mapping must point at that reused bucket.
	e2 := NewEmitter(newParticleTestTemplate(imgB, 1))
	e2.SetEmitting(true)
	r.AddEmitter(e2)
	if len(r.bucketList) != 1 {
		t.Fatalf("buckets = %d, want 1 (retired bucket reused)", len(r.bucketList))
	}
	if len(r.freeList) != 0 {
		t.Fatal("retired bucket was not taken from the free list")
	}
	id, ok := r.bucketIDByImage[imgB]
	if !ok {
		t.Fatal("no mapping for the new texture")
	} else if r.bucketList[id].img != imgB {
		t.Fatal("mapping does not point at the bucket that was reused for the new texture")
	}
	e2.Update()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererParticleCount(r) == 0 {
		t.Fatal("particles of the new emitter are not visible after bucket reuse")
	}
}

func TestRendererDisposeThenAddSameFrame(t *testing.T) {
	t.Run("DifferentTexture", func(t *testing.T) {
		dst := ebiten.NewImage(4, 4)
		imgA := ebiten.NewImage(2, 2)
		imgB := ebiten.NewImage(2, 2)

		r := NewRenderer()
		e1 := NewEmitter(newParticleTestTemplate(imgA, 1))
		e1.SetEmitting(true)
		r.AddEmitter(e1)
		e1.Update()
		r.Draw(dst)

		// Same frame: dispose the old emitter, then add a new one
		// with a different texture, before the next Draw.
		e1.Dispose()
		e2 := NewEmitter(newParticleTestTemplate(imgB, 1))
		e2.SetEmitting(true)
		r.AddEmitter(e2)
		e2.Update()

		r.Draw(dst)
		checkRendererConsistency(t, r)
		for _, p := range bucketContentProblems(r) {
			t.Error(p)
		}
		if activeBucketCount(r) != 1 {
			t.Fatalf("active buckets = %d, want 1", activeBucketCount(r))
		}
		if len(r.bucketIDByImage) != 1 {
			t.Fatalf("mappings = %d, want 1", len(r.bucketIDByImage))
		}
		if _, ok := r.bucketIDByImage[imgA]; ok {
			t.Fatal("stale mapping for the disposed emitter's texture")
		}
		if _, ok := r.bucketIDByImage[imgB]; !ok {
			t.Fatal("missing mapping for the new emitter's texture")
		}
		if rendererEmitterCount(r) != 1 {
			t.Fatalf("visible emitters = %d, want 1", rendererEmitterCount(r))
		}
	})

	t.Run("SameTexture", func(t *testing.T) {
		dst := ebiten.NewImage(4, 4)
		imgA := ebiten.NewImage(2, 2)

		r := NewRenderer()
		e1 := NewEmitter(newParticleTestTemplate(imgA, 1))
		e1.SetEmitting(true)
		r.AddEmitter(e1)
		e1.Update()
		r.Draw(dst)

		// Same frame: dispose the old emitter, then add a new one
		// bound to the same texture.
		e1.Dispose()
		e2 := NewEmitter(newParticleTestTemplate(imgA, 1))
		e2.SetEmitting(true)
		r.AddEmitter(e2)
		e2.Update()

		r.Draw(dst)
		checkRendererConsistency(t, r)
		for _, p := range bucketContentProblems(r) {
			t.Error(p)
		}
		if len(r.bucketList) != 1 {
			t.Fatalf("buckets = %d, want 1 (same texture must reuse the same bucket)", len(r.bucketList))
		}
		if activeBucketCount(r) != 1 {
			t.Fatalf("active buckets = %d, want 1", activeBucketCount(r))
		}
		if rendererEmitterCount(r) != 1 {
			t.Fatalf("visible emitters = %d, want 1 (disposed emitter removed)", rendererEmitterCount(r))
		}
		if rendererParticleCount(r) == 0 {
			t.Fatal("new emitter's particles are not visible")
		}
	})
}

func TestRendererRepeatedDrawKeepsVisibleSet(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	img := ebiten.NewImage(2, 2)

	r := NewRenderer()
	e := NewEmitter(newParticleTestTemplate(img, 2))
	e.SetEmitting(true)
	r.AddEmitter(e)
	for i := 0; i < 6; i++ {
		e.Update()
	}

	r.Draw(dst)
	particles := rendererParticleCount(r)
	emitters := rendererEmitterCount(r)
	buckets := activeBucketCount(r)
	mappings := len(r.bucketIDByImage)
	if particles == 0 {
		t.Fatal("no visible particles before the repeated Draw calls")
	}

	// Drawing is a read-only operation with respect to the visible
	// particle set; repeating it must not change anything.
	for i := 0; i < 3; i++ {
		r.Draw(dst)
		checkRendererConsistency(t, r)
		if rendererParticleCount(r) != particles {
			t.Fatalf("Draw %d changed the visible particle set: %d -> %d", i, particles, rendererParticleCount(r))
		}
		if rendererEmitterCount(r) != emitters || activeBucketCount(r) != buckets {
			t.Fatalf("Draw %d changed the emitter/bucket sets", i)
		}
		if len(r.bucketIDByImage) != mappings {
			t.Fatalf("Draw %d changed the texture->bucket mapping", i)
		}
	}
}

func TestRendererInvisibleEmitterKept(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	img := ebiten.NewImage(2, 2)

	r := NewRenderer()
	e := NewEmitter(newParticleTestTemplate(img, 1))
	e.SetEmitting(true)
	r.AddEmitter(e)
	for i := 0; i < 3; i++ {
		e.Update()
	}
	e.SetVisibility(false)

	// An invisible emitter is not drawn, but it must not be dropped
	// from the visible set while it is still alive.
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererEmitterCount(r) != 1 {
		t.Fatal("invisible (but live) emitter was removed from the visible set")
	}
	if activeBucketCount(r) != 1 {
		t.Fatal("bucket of an invisible live emitter was recycled")
	}
}

func TestRendererMultipleEmittersSameTexture(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	img := ebiten.NewImage(2, 2)
	tmpl := newParticleTestTemplate(img, 1)

	r := NewRenderer()
	e1 := NewEmitter(tmpl)
	e2 := NewEmitter(tmpl)
	e1.SetEmitting(true)
	e2.SetEmitting(true)
	r.AddEmitter(e1)
	r.AddEmitter(e2)
	if len(r.bucketList) != 1 {
		t.Fatalf("buckets = %d, want 1 for two emitters sharing a texture", len(r.bucketList))
	}
	e1.Update()
	e2.Update()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if rendererEmitterCount(r) != 2 {
		t.Fatalf("visible emitters = %d, want 2", rendererEmitterCount(r))
	}

	// Disposing one of the two emitters keeps the bucket alive.
	e1.Dispose()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if activeBucketCount(r) != 1 || rendererEmitterCount(r) != 1 {
		t.Fatal("bucket was recycled while it still had a live emitter")
	}

	// Disposing the last emitter recycles the bucket.
	e2.Dispose()
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if activeBucketCount(r) != 0 || len(r.bucketIDByImage) != 0 {
		t.Fatal("bucket was not recycled after all of its emitters were disposed")
	}
}

func TestRendererInterleavedOperations(t *testing.T) {
	dst := ebiten.NewImage(4, 4)
	imgs := []*ebiten.Image{
		ebiten.NewImage(2, 2),
		ebiten.NewImage(2, 2),
		ebiten.NewImage(2, 2),
	}
	tmpls := make([]*Template, len(imgs))
	for i, img := range imgs {
		tmpls[i] = newParticleTestTemplate(img, 0.2+0.1*float64(i))
	}

	rng := rand.New(rand.NewSource(42))
	r := NewRenderer()
	var emitters []*Emitter

	for step := 0; step < 400; step++ {
		switch rng.Intn(10) {
		case 0, 1, 2: // Add a new emitter with a random texture.
			e := NewEmitter(tmpls[rng.Intn(len(tmpls))])
			e.SetEmitting(true)
			r.AddEmitter(e)
			emitters = append(emitters, e)
		case 3, 4: // Stop a random emitter.
			if len(emitters) != 0 {
				emitters[rng.Intn(len(emitters))].SetEmitting(false)
			}
		case 5: // Restart a random emitter.
			if len(emitters) != 0 {
				emitters[rng.Intn(len(emitters))].SetEmitting(true)
			}
		case 6: // Dispose a random emitter.
			if len(emitters) != 0 {
				emitters[rng.Intn(len(emitters))].Dispose()
			}
		case 7, 8: // Advance the simulation.
			for _, e := range emitters {
				e.Update()
			}
		case 9: // Draw a frame; afterwards all invariants must hold.
			r.Draw(dst)
			checkRendererConsistency(t, r)
			for _, p := range bucketContentProblems(r) {
				t.Errorf("step %d: %s", step, p)
			}
		}
		if t.Failed() {
			t.Fatalf("aborting at step %d", step)
		}
	}

	// Dispose everything; after the final Draw all buckets must be
	// recycled and the mapping must be empty.
	for _, e := range emitters {
		e.Dispose()
	}
	r.Draw(dst)
	checkRendererConsistency(t, r)
	if activeBucketCount(r) != 0 {
		t.Fatal("not all buckets were recycled")
	}
	if len(r.bucketIDByImage) != 0 {
		t.Fatal("texture->bucket mapping is not empty after disposing all emitters")
	}
	if len(r.freeList) != len(r.bucketList) {
		t.Fatalf("free list size = %d, want %d (all buckets)", len(r.freeList), len(r.bucketList))
	}
}

// TestRendererConcurrentUse runs several independent renderers from
// separate goroutines. Renderers share no mutable state, so this must
// be race-free (run with -race) and every renderer must stay consistent.
func TestRendererConcurrentUse(t *testing.T) {
	const numRenderers = 4

	renderers := make([]*Renderer, numRenderers)
	var wg sync.WaitGroup
	for i := 0; i < numRenderers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dst := ebiten.NewImage(4, 4)
			img := ebiten.NewImage(2, 2)
			tmpl := newParticleTestTemplate(img, 0.3)

			r := NewRenderer()
			renderers[i] = r
			var emitters []*Emitter
			for step := 0; step < 60; step++ {
				e := NewEmitter(tmpl)
				e.SetEmitting(true)
				r.AddEmitter(e)
				emitters = append(emitters, e)
				for _, e2 := range emitters {
					e2.Update()
				}
				if step%3 == 0 && len(emitters) > 1 {
					victim := emitters[0]
					victim.SetEmitting(false)
					victim.Dispose()
					emitters = emitters[1:]
				}
				r.Draw(dst)
			}
		}(i)
	}
	wg.Wait()

	for i, r := range renderers {
		for _, p := range rendererConsistencyProblems(r) {
			t.Errorf("renderer %d: %s", i, p)
		}
		for _, p := range bucketContentProblems(r) {
			t.Errorf("renderer %d: %s", i, p)
		}
	}
}
