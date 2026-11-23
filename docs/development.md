# Development

The root `justfile` imports focused recipes from `just/build.just`, `just/dev.just`, and `just/test.just`, plus demo recipes in `just/demo.just`.

```sh
just                   # list recipes
just check             # format check, vet, race tests, build
just test-integration  # generated fixtures + real ffmpeg/mpv; null audio output
just test-tui          # real PTY interaction, resize, controls, process cleanup
just build-all         # macOS/Linux, arm64/amd64; compile only
```

Integration fixtures and terminal transcripts are generated under ignored `tmp/`. Tests use synthetic tones and a null audio device, so they verify playback timing and command behavior without playing sound through your speakers. Actual device output and perceptual audio/video synchronization require a manual listening check.

`just test-explorer`, `just test-file-operations`, `just test-mermaid`, `just test-demo`, and `just test-package` run focused acceptance checks. Python is used only by terminal/package test harnesses; peep, its installer, and its renderer are Go.

The optional `just demo-assets` authoring recipe regenerates original demo art, music, and motion; see [credits and sources](../examples/night-shift/CREDITS.md).

Use feature branches and ready-for-review pull requests. Do not commit generated test fixtures, binaries, or terminal transcripts. Curated demo media in `examples/night-shift` is intentionally versioned.

See [architecture](architecture.md) and [verification](verification.md).

Release packaging and channel setup: [distribution](distribution.md).
