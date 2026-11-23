# Using peep

[Back to peep](../README.md) · [Terminal graphics](terminal-graphics.md)

## Search results from stdin

Pipe filenames into peep to browse just those results:

```sh
rg -l "TODO" | peep
rg -l -0 "TODO" | peep --read0
find . -type f -name '*.md' -print0 | peep --read0
```

Use `rg -l` for filenames; ordinary `rg` output includes matching lines and is
not a file list. Peep reads the complete list before opening the UI. The default
is one path per line (LF or CRLF); `--read0` accepts NUL separators and preserves
embedded newlines and carriage returns. Spaces are preserved in both modes.
Relative paths are resolved from the directory where peep was launched.

Results retain input order, show relative paths to distinguish identical names,
and include explicitly supplied hidden files. Duplicate paths appear once.
Search, previews, Vim navigation, editor handoff, and file operations work as in
the normal browser. `S` chooses a sort; `S` then `i` restores input order.
Opening a listed directory browses that folder; Backspace from that folder
returns to the result list. At the result list, Backspace stays in the list.

Empty input exits without opening the UI. Invalid paths produce an error; files
removed after startup disappear when the list refreshes. Renaming or moving a
result updates its path, and deleting it removes it from the list. Stdin paths
cannot be combined with positional paths. Add `--play` only for a list containing
playable media files. Keyboard input uses `/dev/tty`, so an interactive terminal
is still required; the default mode starts silently.

## Controls

| Key | Action |
|---|---|
| `j/k`, up/down | Select file; select queue entry when queue is open |
| `h/l`, left/right, `Backspace` | Parent / enter directory; return from a piped result's folder |
| `Enter` | Enter directory, focus a document, or start selected media |
| `gg`, `G`, `12G` / `12gg` | First, last, or numbered entry; rendered row in document focus |
| `5j`, `5k`, `Ctrl+D` / `Ctrl+U` | Counted movement; half-page down / up |
| `#` | Toggle absolute / relative file and queue numbers (selected row stays absolute) |
| `PgUp/PgDn` | Page through document preview, otherwise file list or queue |
| `j/k`, arrows, Home/End in document focus | Scroll document / beginning / end |
| `R` on Markdown | Toggle styled Markdown and source |
| `/` or `Ctrl+F`, `Esc` | Search files and folders / go back (close overlay, return from focus, clear filter, then parent) |
| `H` | Toggle hidden files |
| `S` in browser | Sort by name, modified, created, size, or type; reverse order |
| `/` / `Ctrl+F`, then `n` / `N` in document focus | Find text; next / previous match |
| `e` | Open selected file in `$VISUAL`, then `$EDITOR`, or `vi` |
| `Ctrl+R` | Rename selected entry; Ctrl+U clears the current name |
| `M` | Move to a folder or full destination path |
| `d` / `Delete` in browser | Delete with a yes/no confirmation |
| `Space`, `p` | Play/pause |
| `s` | Stop and reset to zero |
| `[`, `]` | Seek backward/forward by the selected step (default 5 seconds); also clickable |
| `t`, `Tab`, then left/right | Focus timeline and seek backward/forward by the selected step |
| Click timeline | Seek to the clicked position |
| `,` | Cycle seek step: 1, 5, 10, 30, 60 seconds |
| `{`, `}` | Seek backward/forward 30 seconds |
| `0` | Seek to start |
| `n`, `b` | Next/previous queue entry |
| `z`, `x`, `c` | Halve/double speed (0.25–4x), reset to 1x |
| `-`, `=`, `m` | Volume down/up, mute |
| `F` | Cycle video/GIF target FPS: 1, 6, 12, 24, 30, 60 |
| `D` | Cycle image display duration: 2, 5, 10, 30 seconds |
| `r` | Repeat off / one / all |
| `a`, `Q` | Append selected file / toggle queue |
| `d` in queue | Remove an inactive queue entry; never its file |
| `f`, `v` | Focus preview / show now playing |
| `B` | Toggle full-width browser |
| `?`, `q`, `Ctrl+C` | Help / quit / quit |

Help groups shortcuts by task and adapts to the terminal width. Use `j`/`k`, arrow keys, Page Up/Down, or the mouse wheel to scroll; Home/End jumps to the first/last section. `?` or Escape closes help. Displayed paths within your home directory use `~`; file operations still use the full path.

Search is immediate and local to the current directory. It matches case-insensitive substrings and fuzzy subsequences; space-separated terms must all match. Queries containing glob syntax instead match the whole filename (for example, `rec-*` or `*.txt`). Results are filtered from the cached listing. Enter keeps the filter; Esc clears it. Directory selection uses the full-width browser rather than an empty preview pane.

File and queue rows have one-based numbers, including the parent entry. Numbers follow the current filtered list. Type colors distinguish directories, media, documents, source, configuration, archives, and PDFs; text badges remain visible. Counts appear beside the current mode and Escape cancels an unfinished command. `Ctrl+F` remains search.

The transport area is hidden for folders and documents unless background media is playing, paused, or loading. Selected media, including image slideshows, retains controls. Finished playback no longer occupies the document reader.

The interface uses a full-color theme, highlighted selection, bordered file/preview/queue panes, and a visible `../ Parent directory` entry. In narrow terminals the browser occupies the main pane; `f` switches to the preview. Click a file to select it, then press Enter to open or play.

During music playback, the now-playing pane shows 24 logarithmic bands across the nominal human hearing range, **20 Hz–20 kHz**, calculated from the actual audio. Labels align with the bars; narrow previews combine bands. Analysis uses a separate stream and does not change playback quality. It follows playback time and seeking, freezes on pause, and clears on stop. Mute and volume affect displayed intensity. This is a visualizer, not an audio equalization effect. Use `v` to return to it while browsing.

Use `--seek-step=500ms`, `--seek-step=1s`, or another positive duration to choose a startup jump size. `,` changes it during the session, and transport labels update immediately. The fixed `{` / `}` and Shift+arrow jumps remain 30 seconds.

Use `--fps=24` to choose the initial video/GIF sampling rate (any integer from 1 to 60; default 12). `F` changes it without restarting playback. The FPS counter shows frames sent to the terminal over the last second versus the target in native mode, or frames rendered in cell mode. It does not measure the terminal display’s refresh rate. A lower target reduces decoding and terminal traffic; actual throughput also depends on playback speed and terminal performance. Late frames are skipped to follow the audio clock.

Timelines always control the file named **NOW PLAYING**, even when the preview shows something else. Images use a slideshow clock, initially five seconds. GIFs play one duration before advancing. Rewind is backward seeking; continuous reverse playback is not implemented. Unknown-duration files have no percentage seek or end jump.

## File operations

Rename, move and delete operate on the selected browser entry; the parent entry is protected. Escape cancels a prompt. Rename accepts a filename; move accepts an existing folder or full path, relative to the current directory, absolute, or beginning with `~/`. Existing destinations (including dangling symlinks) are never overwritten. Moves are atomic within one filesystem; cross-filesystem moves are rejected.

Deletion is permanent. The confirmation shows the target: `y` confirms; `n`, Enter, or Escape cancels. Deleting a folder also deletes all files and subdirectories inside it; the confirmation explicitly warns about this. Symlinks themselves are removed without deleting their targets. Stop playback before modifying the active file or a containing directory. Queued paths are updated after moves and removed after deletion. In the queue, `d` still removes only a queue entry.

The editor command supports quoted paths and arguments, without shell expansion. The file path is passed as a separate absolute argument. For graphical editors, configure their wait flag (for example `EDITOR="code --wait"`). Peep releases the terminal while the editor runs and refreshes the listing and preview on return.

## Document previews

Text and Markdown files have scrollable previews. Enter or `f` focuses a document; Escape returns to the browser. File filtering and playback controls remain available while reading. Reads are bounded to 2 MiB and layout to 12,000 rows, with explicit truncation markers. Non-UTF-8, binary, and special files are not decoded as text. Documents never enter media queues. Remote document resources are not fetched.

Source and configuration files use filename-based syntax highlighting (including Go, Rust, Python, JavaScript/TypeScript, shell, JSON, YAML, TOML and Dockerfiles). Markdown code fences use their language tag, including nested fences. Unknown source types and inputs larger than 128 KiB remain plain text; expensive source-file highlighting falls back to text. Source mode stays unstyled.

Markdown also displays local inline and reference-style images after their containing block. Native graphics preserve pixels and clip images when scrolling; other terminals show color cells. Missing images show an error, and remote images are not fetched. GIFs embedded in documents are still posters. `R` switches back to source.

Mermaid fences and `.mmd`/`.mermaid` files render through the built-in [mmdg Go library](https://github.com/bvolpato/mermaid-go-renderer) when native graphics are available. `R` switches between diagram and source. Invalid diagrams and cell-only terminals keep readable source with an explanation. Top-level fences are replaced by diagrams; diagrams in lists/quotes appear after their containing block, preserving the surrounding text.

The renderer is pinned to v0.13.0 in peep's Go module, with embedded fonts. Peep builds with Go 1.27+ and CGO disabled. Rendering runs in a private worker mode of the same executable so selection changes and timeouts can cancel both layout and PNG conversion. No additional executable is installed or searched on PATH.

Flowcharts, sequence diagrams, and class diagrams are tested. PNGs fit within 1600 × 1600 pixels; the source limit is 128 KiB and the worker deadline is eight seconds. Invalid diagrams retain readable source and an error. No links are opened and no remote image assets are fetched. `just test-mermaid` exercises the built-in renderer; `just test-package` verifies a relocated and installed package with an empty tool PATH.

Focused text previews support `/` or `Ctrl+F` to find text in the displayed document. Search is literal and case-insensitive, with highlighted matches and a match counter. Enter keeps the search; `n` and `N` jump to the next/previous occurrence and wrap at the ends. Escape while typing clears the search and restores the original scroll position; Ctrl+U clears the query. Browser filename filtering stays separate. Use `R` to search Markdown/Mermaid source instead of rendered text.

Symbolic links use a cyan `LINK` badge and filename, including directory links and dangling links; directory links still open normally.

Press `S` in the browser for sorting: `n` name, `m` last modified, `c` date created, `s` size, `t` extension/type, or `r` reverse the current order. Dates start newest first and sizes largest first. Directories stay first; unavailable metadata stays last. Sorting preserves the selected entry and persists across folders and filters during the session. Creation time uses filesystem birth time on macOS and Linux where supported, never inode-change time. Directory sizes describe the directory entry, not recursive contents; symlink metadata describes the link itself.

The browser filter accepts whole-filename globs such as `rec-*`, `*.txt`, `photo-??.jpg`, and `rec-[0-9]*`. Matching is case-insensitive and includes directories. Spaces inside a glob are literal. Plain queries retain ranked substring/fuzzy matching until an explicit sort is chosen. `Ctrl+U` clears the query; malformed glob patterns show an error. Hidden files remain controlled by `H`. The file-list title shows the active sort and direction; the status row shows the selected entry's sort value.

## Interrupted sessions

Normal quit, Ctrl+C, SIGINT, SIGTERM and SIGHUP stop peep’s media workers and restore terminal modes. Editors receive the real terminal descriptors and peep restores its UI when they exit. An uncatchable `kill -9` cannot run cleanup; if a forced kill leaves a terminal in a bad state, run `reset` in that terminal.
