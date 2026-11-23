package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"text/template"

	"github.com/alfg/mp4"
	"github.com/alfg/mp4/atom"
)

const tmpl = `
File:
  file size:    {{.Size}}
{{- with .Ftyp}}
  brands:       {{.MajorBrand}} {{.MinorVersion}} {{.CompatibleBrands}}
{{- end}}
{{- with .Moov}}

Movie:
  {{- with .Mvhd}}
  duration:     {{.Duration}} ms / {{.Timescale}} ({{getDurationString .Duration .Timescale}})
  timescale:    {{.Timescale}}
  {{- end}}
  fragments:    {{$.IsFragmented}}

Found {{len .Traks}} Tracks
{{- range $trak := .Traks}}
{{with $trak.Tkhd}}
Track: {{.TrackID}}
  flags:    {{.Flags}} {{getFlags .Flags}}
  id:       {{.TrackID}}
  duration: {{.Duration}} ms
  width:    {{to16 .Width}}
  height:   {{to16 .Height}}
  {{- if (or (ne .GetWidth 0) (ne .GetHeight 0)) }}
    display width:  {{.GetWidth}}
    display height: {{.GetHeight}}
  {{- end}}
{{- end}}
{{- with $trak.Mdia}}{{$mdia := .}}
  {{- with .Hdlr}}
  type:     {{getHandlerType .Handler}}
  {{- end}}
  {{- with .Mdhd}}
  language: {{.LanguageString}}
  media:
    timescale:      {{.Timescale}}
    duration:       {{.Duration}} (media timescale units)
    duration:       {{getDurationMS .Duration .Timescale}} (ms)
  {{- end}}
  {{- with .Minf}}
    {{- with .Stbl}}
      {{- with .Stts}}
        {{- if .SampleCounts}}
    sample count:   {{index .SampleCounts 0}}
        {{- end}}
        {{- if and $mdia.Hdlr $mdia.Mdhd}}
          {{- if (eq (getHandlerType $mdia.Hdlr.Handler) "Video")}}
    frame rate (computed): {{getFramerate .SampleCounts $mdia.Mdhd.Duration $mdia.Mdhd.Timescale}}
          {{- end}}
        {{- end}}
      {{- end}}
    {{- end}}
  {{- end}}
{{- end}}
{{- end}}
{{- end}}
`

var input string

func init() {
	flag.StringVar(&input, "i", "", "-i input_file.mp4")
	flag.Parse()
}

func main() {
	if input == "" {
		flag.Usage()
		return
	}

	f, err := mp4.Open(input)
	if err != nil && f == nil {
		// Total failure: nothing could be parsed.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err != nil {
		// Partial success: keep going with whatever parsed cleanly.
		fmt.Fprintln(os.Stderr, "warning:", err)
	}

	// defer f.Close()

	funcMap := template.FuncMap{
		"getDurationString": atom.GetDurationString,
		"getFlags":          getFlags,
		"getHandlerType":    getHandlerType,
		"to16":              to16,
		"getDurationMS":     getDurationMS,
		"getFramerate":      getFramerate,
	}

	t := template.Must(template.New("tmpl").Funcs(funcMap).Parse(tmpl))
	if err := t.Execute(os.Stdout, f); err != nil {
		fmt.Fprintln(os.Stderr, "template error:", err)
	}

	if len(f.Errors) > 0 {
		fmt.Println("\nFailed boxes:")
		for _, be := range f.Errors {
			fmt.Println(" ", be)
		}
	}
}

func getFramerate(sampleCounts []uint32, duration, timescale uint32) string {
	if len(sampleCounts) == 0 || duration == 0 || timescale == 0 {
		return "n/a"
	}
	sc := 1000 * sampleCounts[0]
	durationMS := math.Floor(float64(duration) / float64(timescale) * 1000)
	if durationMS == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(sc)/durationMS)
}

func getDurationMS(duration, timescale uint32) string {
	if timescale == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", math.Floor(float64(duration)/float64(timescale)*1000))
}

func to16(i atom.Fixed32) int {
	return int(i / (1 << 16))
}

func getHandlerType(handler string) string {
	var t string
	if handler == "vide" {
		t = "Video"
	} else if handler == "soun" {
		t = "Sound"
	}
	return t
}

func getFlags(flags uint32) string {
	var f []string
	if flags&atom.TrackFlagEnabled == atom.TrackFlagEnabled {
		f = append(f, "ENABLED")
	}

	if flags&atom.TrackFlagInMovie == atom.TrackFlagInMovie {
		f = append(f, "IN-MOVIE")
	}

	if flags&atom.TrackFlagInPreview == atom.TrackFlagInPreview {
		f = append(f, "IN-PREVIEW")
	}
	str := strings.Join(f, " ")
	return str
}
