package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/alexflint/go-arg"
)

var Commit = func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}

	return ""
}()

type args struct {
	Rom        string `arg:"positional,required" help:"Path to the ROM file"`
	Screenshot string `arg:"required,--img" help:"path of in-game screenshot" placeholder:"<SCREENSHOT>"`
	Output     string `arg:"--output" help:"output file" default:"out.png" placeholder:"<FILE>"`
}

func (args) Description() string {
	return "GBGraphics - extract graphics from Gameboy ROM using a screenshot"
}

func (args) Version() string {
	return "Version (git commit):" + Commit
}

// run executes one extraction batch. It validates every input and decodes and
// encodes every tile in memory before anything is written to disk, then
// publishes the whole batch atomically. Any error is returned before a partial
// result set can be left behind.
func run(userInput args) error {
	path := userInput.Rom
	if path == "" {
		return errors.New("no ROM specified")
	}

	romBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read ROM: %w", err)
	}

	// Load and validate the screenshot before doing any work.
	img, err := readImageFromFilePath(userInput.Screenshot)
	if err != nil {
		return err
	}

	if img.Bounds().Dx() != gbScreenXRes || img.Bounds().Dy() != gbScreenYRes {
		return fmt.Errorf("screenshot is not %dx%d, it is %dx%d",
			gbScreenXRes, gbScreenYRes, img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Collect every matching address (loop from 0 to 7 column offsets).
	var allAddresses []string
	for i := 0; i < 8; i++ {
		locations, err := getTiles(img, romBytes, i)
		if err != nil {
			return err
		}
		allAddresses = append(allAddresses, locations...)
	}

	uniqueAddresses := removeDuplicateString(allAddresses)

	// Decode and encode every tile in memory first. If any address is bad,
	// this fails here — before a single output file is created.
	outputs, err := prepareOutputs(uniqueAddresses, userInput.Output, romBytes)
	if err != nil {
		return err
	}

	// Publish the whole batch atomically.
	if err := writeBatch(outputs, userInput.Output); err != nil {
		return err
	}

	for i, address := range uniqueAddresses {
		fmt.Printf("Tile found at %s written to '%s'\n", address, outputs[i].filename)
	}

	return nil
}

func main() {
	var userInput args

	arg.MustParse(&userInput)

	if err := run(userInput); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
