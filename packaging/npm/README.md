# peep

Your files. In focus. Browse files, preview images and video, listen to music and audiobooks, and read Markdown, source code, and Mermaid diagrams in your terminal.

```sh
npm install -g @obedience-corp/peep
peep ~/Pictures
peep --doctor
```

macOS and Linux on x64 and arm64 are supported. The matching native executable is installed through npm's optional platform dependencies. No Go compiler, postinstall download, Node-based diagram renderer, or separate Mermaid helper is needed. Node is used only to launch the Go executable.

For media playback, install FFmpeg and mpv separately:

- macOS: `brew install ffmpeg mpv`
- Arch Linux: `sudo pacman -S ffmpeg mpv`
- Debian/Ubuntu: `sudo apt install ffmpeg mpv`

`peep --doctor` reports missing tools. Documents and Mermaid work without them. Native image quality requires a Kitty-graphics-compatible terminal; other terminals use color-cell previews.

[Source and full guide](https://github.com/lancekrogers/peep)
