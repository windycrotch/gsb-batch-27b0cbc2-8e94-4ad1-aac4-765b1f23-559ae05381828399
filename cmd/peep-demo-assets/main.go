// peep-demo-assets creates original artwork and an original instrumental score.
// Writes only to the requested output directory; FFmpeg encodes distribution media.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "tmp/demo-assets", "output directory")
	flag.Parse()
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate(out string) error {
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	for _, name := range []string{"moonrise", "daybreak"} {
		img := landscape(1600, 1000, name == "daybreak")
		f, err := os.Create(filepath.Join(out, name+".png"))
		if err != nil {
			return err
		}
		err = png.Encode(f, img)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if name == "moonrise" {
			f, err = os.Create(filepath.Join(out, "moonrise.jpg"))
			if err != nil {
				return err
			}
			err = jpeg.Encode(f, img, &jpeg.Options{Quality: 94})
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return music(filepath.Join(out, "night-shift.wav"))
}
func mix(a, b color.RGBA, t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	return color.RGBA{uint8(float64(a.R)*(1-t) + float64(b.R)*t), uint8(float64(a.G)*(1-t) + float64(b.G)*t), uint8(float64(a.B)*(1-t) + float64(b.B)*t), 255}
}
func landscape(w, h int, dawn bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	top, bottom := color.RGBA{9, 16, 35, 255}, color.RGBA{47, 91, 110, 255}
	if dawn {
		top, bottom = color.RGBA{33, 39, 75, 255}, color.RGBA{241, 169, 118, 255}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := float64(y) / float64(h)
			c := mix(top, bottom, v)
			glow := math.Exp(-math.Pow((float64(x)/float64(w)-.7)*3, 2)-math.Pow((v-.42)*4, 2)) * .16
			c = mix(c, color.RGBA{112, 221, 192, 255}, glow)
			img.SetRGBA(x, y, c)
		}
	}
	rng := rand.New(rand.NewSource(2026))
	for i := 0; i < 500; i++ {
		x, y := rng.Intn(w), rng.Intn(h*6/10)
		light := uint8(110 + rng.Intn(140))
		if dawn {
			light /= 2
		}
		c := color.RGBA{light, light, light, 255}
		img.SetRGBA(x, y, c)
		if i%19 == 0 {
			for d := -3; d <= 3; d++ {
				img.SetRGBA(x+d, y, mix(img.RGBAAt(x+d, y), c, .5))
				img.SetRGBA(x, y+d, mix(img.RGBAAt(x, y+d), c, .5))
			}
		}
	}
	cx, cy, r := w*7/10, h*28/100, h*9/100
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := float64(x-cx)/float64(r), float64(y-cy)/float64(r)
			if dx*dx+dy*dy <= 1 {
				c := mix(color.RGBA{176, 207, 202, 255}, color.RGBA{240, 236, 209, 255}, .55+.18*math.Sin(dx*13+dy*7)*math.Sin(dy*19))
				if dawn {
					c = color.RGBA{255, 220, 156, 255}
				}
				img.SetRGBA(x, y, c)
			}
		}
	}
	layers := []color.RGBA{{42, 71, 89, 255}, {29, 52, 71, 255}, {17, 35, 50, 255}, {10, 24, 36, 255}}
	for layer, c := range layers {
		if dawn {
			c = mix(c, color.RGBA{131, 85, 93, 255}, float64(4-layer)/7)
		}
		for x := 0; x < w; x++ {
			nx := float64(x) / float64(w)
			ridge := .56 + float64(layer)*.1 + .055*math.Sin(nx*13+float64(layer)*2) + .025*math.Sin(nx*31+float64(layer)) + .008*math.Sin(nx*89)
			y0 := int(ridge * float64(h))
			for y := y0; y < h; y++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	// An observatory silhouette: dome, low building, and a warm window.
	bx, by := w*24/100, h*73/100
	for y := by - 24; y < by+22; y++ {
		for x := bx - 42; x < bx+42; x++ {
			img.SetRGBA(x, y, color.RGBA{8, 20, 31, 255})
		}
	}
	for y := by - 62; y < by-24; y++ {
		for x := bx - 40; x < bx+40; x++ {
			dx, dy := x-bx, y-(by-24)
			if dx*dx+dy*dy < 40*40 {
				img.SetRGBA(x, y, color.RGBA{8, 20, 31, 255})
			}
		}
	}
	for y := by - 10; y < by+1; y++ {
		for x := bx + 15; x < bx+25; x++ {
			img.SetRGBA(x, y, color.RGBA{244, 192, 112, 255})
		}
	}
	return img
}
func music(path string) error {
	const rate = 44100
	const bpm = 96.0
	const beat = 60 / bpm
	const duration = 40.0 // sixteen bars, four chords, a complete loop
	frames := int(rate * duration)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	size := uint32(frames * 4)
	header := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x02\x00\x44\xac\x00\x00\x10\xb1\x02\x00\x04\x00\x10\x00data\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(header[4:], size+36)
	binary.LittleEndian.PutUint32(header[40:], size)
	if _, err = f.Write(header); err != nil {
		return err
	}
	chords := [][4]int{{57, 60, 64, 67}, {53, 57, 60, 64}, {48, 55, 60, 64}, {55, 59, 62, 67}}
	rng := rand.New(rand.NewSource(17))
	buf := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		t := float64(i) / rate
		chord := chords[int(t/(4*beat))%4]
		left, right := 0.0, 0.0
		// Sustained triads, slightly detuned and stereo-spread.
		for j, n := range chord {
			hz := frequency(n)
			chordAge := math.Mod(t, 4*beat)
			gain := .035 * (1 + .15*math.Sin(t*.7)) * math.Min(1, chordAge/.08) * math.Min(1, (4*beat-chordAge)/.08)
			left += gain * math.Sin(2*math.Pi*hz*t)
			right += gain * math.Sin(2*math.Pi*hz*1.001*t+float64(j)*.2)
		}
		step := int(t / (beat / 2))
		age := math.Mod(t, beat/2)
		note := chord[[]int{0, 2, 1, 3, 2, 1, 3, 2}[step%8]] + 12
		hz := frequency(note)
		pluck := .13 * (1 - math.Exp(-age*150)) * math.Exp(-age*9) * (math.Sin(2*math.Pi*hz*age) + .2*math.Sin(4*math.Pi*hz*age))
		pan := .5 + .3*math.Sin(float64(step)*1.7)
		left += pluck * (1 - pan)
		right += pluck * pan
		bassAge := math.Mod(t, beat)
		bass := .11 * (1 - math.Exp(-bassAge*200)) * math.Exp(-bassAge*2) * math.Sin(2*math.Pi*frequency(chord[0]-24)*t)
		kickAge := math.Mod(t, beat*2)
		kick := .2 * math.Exp(-kickAge*19) * math.Sin(2*math.Pi*(48*kickAge+2*(1-math.Exp(-kickAge*25))))
		snareAge := math.Mod(t+beat, beat*2)
		noise := rng.Float64()*2 - 1
		snare := noise * .09 * math.Exp(-snareAge*35)
		hat := noise * .027 * math.Exp(-age*80)
		fade := math.Min(1, t/1.5) * math.Min(1, (duration-t)/2.5)
		left = (left + bass + kick + snare + hat) * fade
		right = (right + bass + kick + snare + hat) * fade
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(int16(math.Tanh(left)*26000)))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(int16(math.Tanh(right)*26000)))
	}
	_, err = f.Write(buf)
	return err
}
func frequency(note int) float64 { return 440 * math.Pow(2, float64(note-69)/12) }
