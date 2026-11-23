#!/usr/bin/env bash
set -euo pipefail
mkdir -p tmp/fixtures
ffmpeg -v error -y -f lavfi -i 'testsrc2=size=160x96:rate=12:duration=8' -f lavfi -i 'sine=frequency=440:duration=8' -c:v mpeg4 -q:v 5 -c:a aac -shortest tmp/fixtures/03-video.mp4
ffmpeg -v error -y -f lavfi -i 'testsrc2=size=160x96:rate=1' -frames:v 1 tmp/fixtures/01-image.png
ffmpeg -v error -y -f lavfi -i 'testsrc2=size=160x96:rate=8:duration=3' tmp/fixtures/02-animation.gif
ffmpeg -v error -y -f lavfi -i 'sine=frequency=330:duration=8' -metadata title='Peep test tone' -metadata artist='Synthetic fixture' tmp/fixtures/04-audio.wav
printf 'not an image\n' > tmp/fixtures/05-broken.png
mkdir -p tmp/fixtures/folder

ffmpeg -v error -y -i tmp/fixtures/01-image.png -frames:v 1 tmp/fixtures/06-photo.jpg

ffmpeg -v error -y -f lavfi -i 'testsrc2=size=1200x800:rate=1' -frames:v 1 tmp/fixtures/07-detail.jpg

ffmpeg -v error -y -f lavfi -i 'testsrc2=size=1280x720:rate=12:duration=2' -c:v mpeg4 -q:v 5 tmp/fixtures/08-detail.mp4
