# citychime

A menu bar clock that strikes a chime every quarter hour.

- On the hour it strikes following a 12 hour clock: midnight and noon strike
  twelve times, 13:00 strikes once, and so on.
- At :15, :30 and :45 it strikes once.
- Only one chime sequence can play at a time. A trigger that arrives while a
  chime is playing is queued in a single slot (further triggers are dropped), so
  two chimes can never overlap.
- Quitting interrupts a chime that is playing and waits (bounded) for playback
  and scheduling goroutines to stop before the process exits.
- A missing audio file or icon does not stop the program: the tray still
  appears, the problem is shown in the tray tooltip and status item and written
  to the log, and the resource is retried on every trigger. Once it is available
  again the next chime plays normally.

## Build

```bash
go build ./...
go test ./...
```

Create the macOS app bundle:

```bash
./build.sh
```

The bundle expects `chime.mp3` and `icon.png` in `CityHallClock.app/Contents/Resources`.

## Icons

1. Put icons into `AppIcon.iconset`
2. Run the following command
```bash
iconutil -c icns AppIcon.iconset
```

## Layout

- `main.go` – tray UI, lifecycle and quit/shutdown handling.
- `audio.go` – MP3 decoding and beep speaker playback (completion is reported by
  the player, never estimated with a sleep).
- `icon.go` – icon loading with a generated fallback.
- `internal/chime` – the scheduling engine: serialised playback, queue/drop
  policy, cancellation, resource retry and the strike-count rules. It has no
  audio or GUI dependency and is covered by `go test`.
