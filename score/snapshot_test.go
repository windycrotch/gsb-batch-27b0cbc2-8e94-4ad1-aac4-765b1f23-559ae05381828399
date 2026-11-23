package score

import (
	"math/big"
	"sync"
	"testing"

	"github.com/sqweek/sqribe/plumb"

	. "github.com/sqweek/sqribe/core/types"
)

func TestSnapshotSerialisesWithUpdates(t *testing.T) {
	sc := MkScore(plumb.MkPort())
	defer sc.Close()
	sc.LoadBeats([]FrameN{0, 100, 200})
	staff := MkStaff("t", &TrebleClef, 0)
	sc.AddStaff(staff)
	note := &Note{Pitch: 60, Duration: big.NewRat(1, 4), Beat: sc.Head, Offset: big.NewRat(0, 1)}
	sc.AddNotes(staff, note)

	var wg sync.WaitGroup
	// concurrent snapshots must always observe a self-consistent score
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				sc.Snapshot(func(s *Score) {
					if len(s.BeatFrames()) == 0 {
						t.Error("snapshot saw empty beat list")
					}
					next := s.Iter(FrameRange{Min: 0, Max: 100000})
					for next != nil {
						_, next = next()
					}
				})
			}
		}()
	}
	// concurrent edits
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 200; k++ {
			sc.MvBeat(sc.Head.Next(), FrameN(100+k%50))
		}
	}()
	wg.Wait()
}
