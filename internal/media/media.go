// Package media probes local media and decodes bounded RGB frame streams.
package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	Image Kind = "IMAGE"
	GIF   Kind = "GIF"
	Video Kind = "VIDEO"
	Audio Kind = "AUDIO"
)

func Classify(path string) Kind {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp":
		return Image
	case ".gif":
		return GIF
	case ".mp4", ".mkv", ".mov", ".webm", ".avi", ".m4v":
		return Video
	case ".mp3", ".flac", ".wav", ".ogg", ".opus", ".m4a", ".m4b", ".aac":
		return Audio
	}
	return ""
}

type Info struct {
	Path                 string
	Kind                 Kind
	Duration             float64
	Width, Height        int
	Title, Artist, Album string
	HasVisual            bool
}

func Probe(ctx context.Context, path string) (Info, error) {
	info := Info{Path: path, Kind: Classify(path)}
	if info.Kind == "" {
		return info, fmt.Errorf("unsupported media: %s", filepath.Base(path))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		return info, fmt.Errorf("probe %s (ffprobe required): %w", filepath.Base(path), err)
	}
	var data struct {
		Format struct {
			Duration string
			Tags     map[string]string
		}
		Streams []struct {
			CodecType     string `json:"codec_type"`
			Width, Height int
			Duration      string
		}
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return info, fmt.Errorf("decode metadata: %w", err)
	}
	info.Duration, _ = strconv.ParseFloat(data.Format.Duration, 64)
	hasAudio := false
	for _, s := range data.Streams {
		if s.CodecType == "audio" {
			hasAudio = true
		}
		if s.CodecType == "video" && !info.HasVisual {
			info.HasVisual = true
			info.Width = s.Width
			info.Height = s.Height
		}
		if info.Duration <= 0 {
			info.Duration, _ = strconv.ParseFloat(s.Duration, 64)
		}
	}
	for k, v := range data.Format.Tags {
		switch strings.ToLower(k) {
		case "title":
			info.Title = v
		case "artist":
			info.Artist = v
		case "album":
			info.Album = v
		}
	}
	if info.Kind != Audio && (!info.HasVisual || info.Width <= 0 || info.Height <= 0) {
		return info, fmt.Errorf("no decodable visual stream in %s", filepath.Base(path))
	}
	if info.Kind == Audio && !hasAudio {
		return info, fmt.Errorf("no audio stream in %s", filepath.Base(path))
	}
	if info.Kind == Image {
		info.Duration = 5
	}
	return info, nil
}
