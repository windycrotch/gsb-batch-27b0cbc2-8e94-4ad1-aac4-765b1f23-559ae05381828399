# mjpeg

![Build Status](https://github.com/icza/mjpeg/actions/workflows/go.yml/badge.svg)
[![Go Reference](https://pkg.go.dev/badge/github.com/icza/mjpeg.svg)](https://pkg.go.dev/github.com/icza/mjpeg)
[![Go Report Card](https://goreportcard.com/badge/github.com/icza/mjpeg)](https://goreportcard.com/report/github.com/icza/mjpeg)

MJPEG video writer implementation in Go.

## Examples

Let's see an example how to turn the JPEG files `1.jpg`, `2.jpg`, ..., `10.jpg` into a movie file:

    checkErr := func(err error) {
        if err != nil {
            panic(err)
        }
    }

    // Video size: 200x100 pixels, FPS: 2
    aw, err := mjpeg.New("test.avi", 200, 100, 2)
    checkErr(err)

    // Create a movie from images: 1.jpg, 2.jpg, ..., 10.jpg
    for i := 1; i <= 10; i++ {
        data, err := ioutil.ReadFile(fmt.Sprintf("%d.jpg", i))
        checkErr(err)
        checkErr(aw.AddFrame(data))
    }

    checkErr(aw.Close())

Example to add an `image.Image` as a frame to the video:

    aw, err := mjpeg.New("test.avi", 200, 100, 2)
    checkErr(err)

    var img image.Image
    // Acquire / initialize image, e.g.:
    // img = image.NewRGBA(image.Rect(0, 0, 200, 100))

    buf := &bytes.Buffer{}
    checkErr(jpeg.Encode(buf, img, nil))
    checkErr(aw.AddFrame(buf.Bytes()))

    checkErr(aw.Close())

## Concurrency and error handling

`AviWriter` is safe for concurrent use: `AddFrame` and `Close` may be called from
multiple goroutines.

- Frames are written in the order in which their `AddFrame` calls are serialized
  internally. A successful `AddFrame` returns only after the frame data and its
  index entry have been written, so the frame order in the file matches the order
  of successful returns.
- If writing a frame fails, that frame is rolled back (the file contains only
  complete frames), the error is returned, and all subsequent `AddFrame` calls
  keep failing without touching the file. `Close` reports that error too, while
  still finalizing the successfully written frames.
- `Close` is idempotent: repeated calls do not panic and return the result of the
  first call. `AddFrame` calls after `Close` return `ErrClosed`.
- The temporary `.idx_` index file is always removed, both on the normal path
  (after `Close`) and when finalizing (backfilling the frame counts or splicing
  the index) fails. If `New` fails, neither the avi file nor the `.idx_` file is
  left behind.
