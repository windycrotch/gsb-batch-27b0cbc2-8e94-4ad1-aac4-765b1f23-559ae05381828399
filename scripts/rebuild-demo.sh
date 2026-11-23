#!/usr/bin/env bash
# Rebuild the curated media from original sources. Never downloads assets.
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in go ffmpeg rsvg-convert; do command -v "$tool" >/dev/null || { echo "Missing authoring tool: $tool" >&2; exit 1; }; done
root=examples/night-shift
stage=tmp/demo-assets
mkdir -p "$stage"
go run ./cmd/peep-demo-assets --out "$stage"
cp "$stage/moonrise.jpg" "$root/01-images/01-moonrise.jpg"
cp "$stage/daybreak.png" "$root/01-images/02-daybreak.png"
rsvg-convert "$root/source/cover.svg" -o "$root/03-music/cover.png"
rsvg-convert "$root/source/satellite.svg" -o "$stage/satellite.png"
ffmpeg -hide_banner -loglevel error -y -i "$stage/night-shift.wav" -i "$root/03-music/cover.png" -map 0:a -map 1:v -c:a aac -b:a 160k -c:v png -disposition:v attached_pic -metadata title='Night Shift' -metadata artist='peep' -metadata album='Observatory Sessions' "$root/03-music/01-night-shift.m4a"
ffmpeg -hide_banner -loglevel error -y -loop 1 -framerate 12 -i "$stage/moonrise.png" -loop 1 -framerate 12 -i "$stage/satellite.png" -i "$stage/night-shift.wav" -filter_complex "[0:v][1:v]overlay=x='(W+w)*t/24-w':y='190+60*sin(t/5)',scale=1280:800[v]" -map '[v]' -map 2:a -t 24 -c:v libx264 -pix_fmt yuv420p -crf 20 -c:a aac -b:a 128k -af 'afade=t=out:st=21:d=3' -movflags +faststart -metadata title='The night pass' "$root/02-motion/rec-night-pass.mp4"
ffmpeg -hide_banner -loglevel error -y -i "$root/02-motion/rec-night-pass.mp4" -t 12 -filter_complex '[0:v]fps=10,scale=640:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96[p];[b][p]paletteuse=dither=bayer:bayer_scale=3' -loop 0 "$root/02-motion/rec-satellite.gif"
# Narration regeneration is optional. Committed chapters work on all supported hosts.
if [[ "${NARRATE:-0}" == 1 ]]; then
  command -v say >/dev/null || { echo 'Narration authoring requires macOS say.' >&2; exit 1; }
  for chapter in 01 02; do
    say -v Samantha -r 155 -f "$root/source/chapter-$chapter.txt" -o "$stage/chapter-$chapter.aiff"
    ffmpeg -hide_banner -loglevel error -y -i "$stage/chapter-$chapter.aiff" -i "$root/03-music/cover.png" -map 0:a -map 1:v -c:a aac -b:a 80k -c:v png -disposition:v attached_pic -metadata title="Night Shift — Chapter $chapter" -metadata artist='peep · synthetic narration' -metadata album='Night Shift: Field Notes' -f ipod "$root/04-audiobook/chapter-$chapter.m4b"
  done
fi
