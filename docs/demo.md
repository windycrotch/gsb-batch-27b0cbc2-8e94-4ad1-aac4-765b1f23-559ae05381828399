# Demo walkthrough

Run `just demo` from the repository. It builds peep and opens a fresh copy of
`examples/night-shift` under `tmp/`. Nothing needs to be downloaded. Media audio
plays on the machine running peep; use a local session when recording sound.

Use a terminal with working Kitty graphics for the clearest images and diagrams.
The preview heading should say **native**. A 120-column, 36-row window gives the
browser and media room. Avoid forcing `--renderer=kitty` on an unsupported client.

## A short tour

1. **Open the field guide.** Press Enter to read `00-start-here.md`. Scroll past
   the illustration to the Mermaid flowchart. Press `R` to reveal its source, then
   `R` again to render it. Search `/signal`, Enter, then `n` / `N`. Escape returns
   to the browser.
2. **Start the soundtrack.** Enter `03-music`, select `01-night-shift.m4a`, and
   press Enter. Press `v` to show the spectrum. Pause/resume with Space; seek with
   `[` / `]`. The bars come from the actual music.
3. **Keep browsing.** Backspace returns to the collection. Open `01-images` and
   select Moonrise, then Daybreak. Music continues while you browse. Use `f` for
   a full preview and Escape to return.
4. **Show motion.** Open `02-motion`, play the MP4, pause it, change the seek step
   with `,`, and seek. Play the GIF, let it finish, then replay it with Enter.
5. **Listen to the story.** Open `04-audiobook` and play a chapter. Use `x` for
   faster playback and `c` for normal speed. The transcript is available alongside
   the recordings. `n` advances to the next queued chapter.
6. **Browse like a terminal user.** In `02-motion`, try `/rec-*`; in `06-code`,
   try `/*.go`. Enter keeps a filter; Escape clears it. `S`, then `m` sorts by
   modification date; `S`, then `r` reverses it. In `01-images`, the disposable
   copy has deliberately different modification dates.
7. **Finish with file operations.** Stop playback with `s`. On a sample in the disposable copy, show
   Ctrl+R rename or `M` move. `d` opens a compact confirmation; `n` cancels and
   `y` deletes. A fresh `just demo` restores the collection for another take.

## What is included

| Content | File / folder |
| --- | --- |
| JPEG and PNG artwork | `01-images/` |
| H.264/AAC video and looping GIF | `02-motion/` |
| 40-second original stereo music, tags, and cover art | `03-music/` |
| Two synthetic-narration M4B chapters and transcripts | `04-audiobook/` |
| Flowchart, sequence, and class diagrams | `05-diagrams/` |
| Go, JSON, and YAML syntax examples | `06-code/` |
| Markdown with inline/reference images, Mermaid, tables, and code | `00-start-here.md` |
| Symlink and hidden-file browsing | `field-guide-link.md`, `images-link`, `.hidden-field-note.txt` |

Creation dates depend on the filesystem and reflect the copied files; the demo
only sets example **modification** dates. Audiobook chapters are separate files,
not an embedded chapter-marker navigation feature. Still artwork is illustrated,
not photography. See the [credits](../examples/night-shift/CREDITS.md).

## Authoring assets

`just demo-assets` rebuilds artwork, music, cover art, video, and GIF using Go,
FFmpeg, and `rsvg-convert`. Set `NARRATE=1` on macOS to regenerate the audiobook
with `say`. Those tools are for maintainers; the committed collection is already
playable. No authoring step is part of a normal build or install.
