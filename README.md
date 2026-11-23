<p align="center">
  <img src="docs/assets/peep-logo.svg" width="390" alt="peep">
</p>
<p align="center"><strong>Your files. In focus.</strong></p>
<p align="center">
  <a href="https://github.com/Obedience-Corp/festival"><img src="https://img.shields.io/badge/built%20with-Festival-7c3aed.svg" alt="Built with Festival"></a>
  <a href="https://github.com/lancekrogers/peep/actions/workflows/check.yml"><img src="https://img.shields.io/badge/CI-GitHub_Actions-58d6be?style=flat" alt="CI on GitHub Actions"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-58d6be.svg" alt="MIT license"></a>
  <img src="https://img.shields.io/badge/Go-1.27%2B-00ADD8?style=flat" alt="Go 1.27 or newer">
  <img src="https://img.shields.io/badge/platforms-macOS%20%7C%20Linux-58d6be?style=flat" alt="macOS and Linux">
</p>

**peep is a terminal file explorer that lets you see, hear, and read your files.**
Browse a folder, preview an image, play a recording, or open an illustrated
Markdown document—without leaving the terminal.

https://github.com/user-attachments/assets/7cfbda66-c8d0-4ab9-9766-4a7b1b02f94b

*74-second demo watch with sound on.*

- **Browse** with Vim navigation, numbered jumps, sorting, fuzzy search, and globs.
- **Watch** images, animated GIFs, and videos with native terminal graphics.
- **Listen** to music and audiobooks with seeking, playback speed, and a live spectrum.
- **Read** syntax-highlighted source, Markdown with local images, and Mermaid diagrams.
- **Manage** files with rename, move, confirmed deletion, and your preferred editor.

## Get started

Choose an install method:

**Homebrew · macOS / Linux**

```sh
brew install lancekrogers/tap/peep
```

**npm · macOS / Linux** (Node 22+)

```sh
npm install -g @obedience-corp/peep
```

**AUR · Arch Linux**

```sh
yay -S peep-tui-bin
# or: paru -S peep-tui-bin
```

Homebrew and AUR install FFmpeg and mpv for media playback. With npm, install
those tools separately through your system package manager. Mermaid is built in.

```sh
peep              # Browse the current directory
peep ~/Pictures   # Browse a directory (peep <path>)
peep README.md     # Preview a file
rg -l "TODO" | peep # Browse files matching a content search
peep --doctor     # Check media dependencies and terminal support
```

[Installation details and release archives →](docs/install.md)

<details>
<summary>Install from source</summary>

Requires **Go 1.27+** and **just**. `just install` sets up missing media tools
through your supported package manager and upgrades an active legacy Go-bin copy.
Follow the installer's PATH guidance if needed.

```sh
git clone https://github.com/lancekrogers/peep.git
cd peep
just install
peep
```

</details>

## Take a peep

The included **Night Shift** collection has original artwork, a music track,
two narrated audiobook chapters, a video, a GIF, diagrams, and an illustrated field guide.

![Moonrise at North Ridge, from the Night Shift demo](examples/night-shift/01-images/01-moonrise.jpg)

```sh
just demo
```

This opens a disposable copy—safe for showing rename, move, and delete.
The curated files are also ready to browse in [`examples/night-shift`](examples/night-shift).
[Demo walkthrough](docs/demo.md) · [Media credits and sources](examples/night-shift/CREDITS.md)

## The essentials

| Key | Action |
| --- | --- |
| `j/k` or `↑/↓` · `Enter` | Select · open or play |
| `Backspace` · `Esc` | Parent folder · back |
| `/` | Filter filenames; find text inside a focused document |
| `S` | Sort by name, modified, created, size, or type |
| `f` · `v` | Focus preview · show now playing |
| `Space` · `[` / `]` | Play/pause · seek |
| `,` | Change the seek step |
| `?` · `q` | All controls · quit |

Native image quality requires a Kitty-graphics-compatible terminal. Other terminals
use color-cell previews; Mermaid stays readable as source. Audio plays on the host
running peep. [Terminal compatibility →](docs/terminal-graphics.md)

[Full user guide](docs/usage.md) · [Development](docs/development.md) · [Architecture](docs/architecture.md)

[MIT license](LICENSE) · [Distribution and release setup](docs/distribution.md)
