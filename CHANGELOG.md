# CHANGELOG

## develop

- Add `pkg/capture`, a platform independent event driven capture session.
- Rewrite `_example/LoopbackCaptureSharedEventDriven` to use `pkg/capture`. Fixed
  the capture loop hanging after Ctrl-C, the incomplete WAV data, the per-loop
  event watcher goroutines that were never reclaimed, and capture buffers that
  were not released on exit.
- Fix the non-Windows stubs of `IAudioMeterInformation` so the package builds on
  every platform.

## v0.2.0

- Support IMMNotificationClient registration.
- Add `_example/DeviceEvents`.
- Change project layout.
- Use go.mod to manage dependencies.
- Rename `.go` files.

The golang convention doesn't allow the capitalized file name (e.g. `IMMNotificationClient.go`). However, the lower-cased file name (e.g. `immnotificationclient.go`) is very hard to read, I decided to break the convention.

## 0.1.0

Initial version
