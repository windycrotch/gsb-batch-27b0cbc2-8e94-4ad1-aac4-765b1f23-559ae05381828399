# Terminal graphics and current limits

Browse silently, press Enter to play, and keep browsing while the current item continues. Still previews use native Kitty graphics automatically when a direct terminal confirms Kitty graphics support, preserving images up to 2048 pixels per dimension. Other terminals use ANSI color cells. Video and animated GIF playback also use native graphics on that path, preserving up to 1920 × 1080 pixels at a 12 fps target. The color-cell renderer remains the compatibility fallback.


## Native pixels or color cells

The preview title shows `native` or `cells`. Cells draw the image with colored text blocks, so resolution is limited by the terminal grid. Native graphics transmit image pixels through the Kitty protocol. Over SSH, the terminal app on the viewing device must support that protocol; the server's terminal software does not supply that capability. A successful SSH connection and true-color text do not imply native image support.

Auto mode tests support at startup. If the client does not answer or rejects the query, peep keeps the cell fallback. A smaller terminal font can provide more cells, within the decode cap, but cannot match native photo quality. Peep does not yet implement Sixel or iTerm2 image adapters. `--renderer=kitty` forces the protocol only when you know the client supports it; it cannot add support to the client.

## Current limits

- Implemented: directory navigation, hidden-file toggle, filename filtering, still and animated previews, in-terminal video, music playback, tags, and an audio-reactive spectrum, embedded cover art in static previews when FFmpeg exposes it, mixed queues, transport, speed, volume, repeat, and responsive focus/queue views.
- The cell fallback uses a 120-column by 100-pixel decode cap. Native still previews preserve up to 2048 pixels per dimension and are placed by the terminal. Auto mode sends a bounded capability query during startup. Confirmed Kitty-compatible terminals use native graphics; a rejection or no reply within 350 ms keeps cells. Multiplexers skip probing and default to cells. `--renderer=kitty` and `--renderer=cells` bypass detection. No iTerm2 inline-image adapter is implemented. 12 fps is a playback target, not a universal performance guarantee.
- The queue stops with an error on a bad item; use next or select another file. Automatic skip-on-error is deferred. Stopping during load cancels it; Play retries the retained entry.
- Preferences, queue state, and position are session-local. No automatic resume or persistent configuration yet.
- Shuffle, image zoom/pan, custom bindings, timestamp entry, drag scrubbing, queue reordering, subtitles, and network streams are deferred from the broader design.
- Support is based on the installed FFmpeg/mpv build. Filename recognition is not a codec guarantee. Tested fixture details live in [verification](verification.md).

[Back to peep](../README.md)
