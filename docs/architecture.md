# Architecture

Go owns the browser, queue, keyboard controls, transport state, and terminal composition. Bubble Tea v1.3.10 is pinned to the version already used by other campaign tools. Media work runs outside the UI thread.

- `internal/browser`: directory listings, type recognition, filter, and deterministic case-insensitive name order with directories first.
- `internal/media`: cancellable FFprobe metadata and FFmpeg RGB frame streams. The queue holds at most two decoded frames; a pending future frame lives in the UI. Processes receive arguments directly, never through a shell.
- `internal/player`: one isolated mpv process, a private temporary Unix socket, structured JSON commands, property observations, and deterministic process/socket cleanup.
- `internal/render`: ANSI true-color foreground/background pairs using the upper-half-block character; terminal-safe filenames/tags and display-width clipping.
- `internal/tui`: event-loop state, asynchronous listing/loading, preview generations, queue, and layout. Only Bubble Tea writes terminal output.

For moving media, mpv supplies audio and authoritative media time with `--vo=null`. A separate FFmpeg process decodes visual frames at a configurable 1–60 fps (default 12). The UI releases each frame when mpv time reaches its timestamp, drops overdue frames, and restarts a decoder that remains over a second behind after a two-second startup grace period. Seeking or resizing cancels that visual decoder and starts another at the current position. Pausing freezes the transport and limits prefetch to the bounded channel. Still images use a monotonic application clock. A selected poster preview never plays audio.

This deliberately trades duplicate video decoding for a simple, isolated bootstrap path that cannot fight the TUI for stdout. It proves terminal composition and transport without native FFI. It is not the final efficiency story: a shared-decoder backend should be evaluated against the existing interfaces after measurement. Frame timestamps currently use the requested seek offset and the configured resampling cadence; sample-accurate synchronization is not claimed.

Resource limits: 10-second metadata probe, 10-second file-load deadline, 3-second IPC commands, two queued frames, 120 x 100 maximum cell decode size; native PNG limits are described below. Selection, resize, seek, and exit cancel obsolete decoder processes. Preview IDs reject late results. Filename extensions select candidate media types; FFprobe validates actual streams before playback.

Media dependencies remain external installations. Mermaid and its fonts are compiled into peep. The Go executable cross-compiles with CGO disabled; mpv/FFmpeg and a suitable terminal are required for media on the destination. Windows IPC is not implemented.

Future graphics backends must preserve a single terminal writer, cancellation, bounded buffering, and the preview-versus-playback distinction. The broader campaign design remains the product direction; README.md documents what this bootstrap actually implements.

## Native still previews and directory search

`internal/media.PreviewPNG` produces a single PNG with a maximum dimension of 2048 pixels, without upscaling smaller images. A ten-second deadline and 20 MiB output cap bound work. Selection generation IDs reject stale results. This static path covers still images, embedded album art, and video/GIF posters. Native moving playback uses the PNG frame stream described below.

`internal/render.KittyWriter` wraps Bubble Tea's output and implements the terminal file interface, preserving resize detection. A mutex serializes text and Kitty graphics commands. PNG payloads are uploaded in 4096-character base64 chunks, then placed in the preview rectangle after text output. The terminal preserves aspect ratio within that rectangle. Each process owns its image ID; replacement, help overlays, and alternate-screen exit delete its image. Cursor save/restore and `C=1` prevent image placement from moving the TUI cursor. No worker writes to stdout directly.

Auto-selection starts with cells and sends a non-storing Kitty query after Bubble Tea enters raw mode. A positive reply enables native graphics and refreshes the selected/active preview. A negative reply or 350 ms timeout keeps cells. An input wrapper retains the terminal file interface, removes only replies matching the query ID, and forwards unrelated keys. Fragment assembly is bounded in time and bytes. Multiplexers skip probing; explicit renderer overrides bypass it. The startup wrapper keeps capability negotiation outside the media model. The protocol implementation follows [Kitty graphics documentation](https://sw.kovidgoyal.net/kitty/graphics-protocol/). Rendering in a particular terminal still needs user validation; PTY tests verify protocol payloads and lifecycle, not an emulator's actual pixels.

Directory data is loaded once per navigation/hidden-file refresh. Search filters the cached entries with case-insensitive substring and fuzzy-subsequence ranking. Folder selection and active search expand the browser to use available width; the preview pane returns when media is selected or focus is requested.

## Audio-reactive spectrum

A separate cancellable FFmpeg process provides mono 48 kHz PCM for the active music file. A radix-2 FFT over Hann-windowed 8192-sample blocks measures energy in 24 logarithmic bands spanning 20 Hz–20 kHz. Windows overlap with a 2048-sample hop (about 23 updates per second), retaining sub-bass resolution and continuous treble coverage. Levels are mapped from decibels to bar heights; the UI applies decay between observed samples. This is actual file audio, not a simulated animation or microphone capture.

Each block is timestamped against the same media timeline as mpv. The UI consumes it at the corresponding playback position. A two-block channel bounds decode prefetch; seeks cancel and restart the stream at the target. Pause freezes displayed levels; stop/file replacement/exit cancel analysis. Volume and mute scale the display. Analysis failure leaves playback functional and shows a visualizer-specific message. The player does not change audio frequency response.

## Native video and GIF frames

The Kitty path streams PNG video frames at up to 1920 × 1080, sampled at the configured rate. PNG compression level 1 avoids expensive default compression while preserving lossless pixels. FFmpeg keeps smaller sources at their original dimensions. A bounded PNG parser separates frames without losing bytes across pipe reads; two queued frames and a 20 MiB per-frame limit bound buffering. Frame timestamps continue to follow mpv's playback clock.

The terminal compositor refreshes changed native frames even when text is unchanged. A short deferred flush gives the text renderer an opportunity to paint first; a shared lock serializes all writes. Synchronized-output markers enclose deletion, upload, text, and placement to avoid presenting a half-updated frame. Selection, seek, resize, overlays, and exit retain the same generation/cancellation and image-ownership rules. Actual sustained frame rate and graphical-terminal playback remain host-dependent.

## Document preview foundation

The browser now lists ordinary files independently of media recognition. Playable queues still filter through `media.Kind`; CLI `--play` rejects non-media arguments. `internal/document` opens regular files without blocking on FIFOs, reads at most 2 MiB with cancellation checks, validates UTF-8, and sanitizes terminal control characters. A second output sanitizer permits only SGR styling after Markdown entity decoding.

Goldmark parses Markdown structurally and Glamour's ANSI renderer styles complete top-level blocks. The layout retains source-line anchors plus image/diagram references for later composition. Text remains terminal text. Formatting uses a pinned Glamour 0.10 dependency compatible with the existing Bubble Tea API; no major TUI migration is included. Layout is capped at 12,000 rows. Async generation IDs discard stale read/reflow results; resize/source toggle reuse the loaded source and preserve an approximate source anchor. Document focus owns scrolling, while transport continues to own the playing media. Track advancement preserves an open document.


## Embedded document images

Parsed image references resolve relative to the document. Only local regular files are decoded; network and data URLs are rejected. Two cancellable decodes may run at once. Visible images share a 48 MiB PNG budget and at most eight native placements. Offscreen pixels are released while dimensions remain to preserve scroll geometry. Each result carries the document generation. Native placements crop source pixels to the viewport; cells use the same reserved rows. Source mode, overlays, navigation and exit remove owned placements.


## Built-in Mermaid rendering

The mmdg v0.13.0 Go library is compiled into peep, including its fonts; Go 1.27+ is required to build. `internal/diagram` calls `RenderContext`, normalizes the root SVG viewport to fit within 1600 pixels, then calls `SVGToPNG`. The raster API has no context parameter, so `internal/document.Mermaid` starts a private worker mode of the same peep executable. It resolves the executable once and never looks up a renderer on PATH. Worker stdout is exclusively PNG data; only Bubble Tea writes terminal output.

Source is limited to 128 KiB, intermediate SVG to 8 MiB, stdout to 20 MiB, stderr to 4 KiB, and execution to an eight-second parent deadline. Context cancellation kills the worker, including rasterization. The five-second library context bounds layout. A per-application cache holds at most 16 PNGs/32 MiB, keyed by source, executable identity, theme/size, and date. Existing document generation and pixel limits still apply. Invalid diagrams and cell-only clients retain source.

## Installation and archives

`internal/installation` copies the running executable into a temporary version directory, verifies its SHA-256, and atomically changes the public symlink. Versions are content-addressed and retained for running processes. Cancellation or failed activation leaves the existing public executable intact. CLI installation can provision media dependencies through the host package manager; tests use `--no-deps` and isolated prefixes.

`cmd/peep-package` builds a target executable with CGO disabled and creates a tar.gz plus checksum. README, documentation, the curated demo collection, and upstream renderer notices accompany it; notices are also embedded for `--licenses`. Package tests extract and relocate archives, install into a temporary prefix, remove the extracted source, and render through the installed executable with no tools on PATH. Native-protocol PTY tests verify image payloads and process cleanup; they do not prove a terminal emulator's actual pixels.

## FPS and terminal lifecycle

Visible playing video uses a 60 Hz scheduling tick and native refresh can flush at up to 60 Hz; other content retains the 20 Hz UI tick. `F` restarts only the visual decoder at the current media position, preserving the last displayed image while it seeks. Native FPS counts completed new-frame writes over the last second; placement-only redraws do not increment it. Cell FPS counts frames accepted for rendering. Neither is a measurement of physical display refresh.

The command owns SIGINT, SIGTERM and SIGHUP through the shared context, including while Bubble Tea releases the terminal to an editor. Editors receive actual terminal file descriptors so os/exec does not turn graphics/probe wrappers into pipes. When stdin is a path list, the command reads it before startup and opens `/dev/tty` for keyboard input, graphics probing, editor handoff, and saved terminal state. Results use explicit paths rather than a directory scan, retaining input order until the user chooses a sort. After the UI and graphics writer close, a final cleanup resets terminal modes and restores the saved termios state. SIGKILL cannot be handled.
