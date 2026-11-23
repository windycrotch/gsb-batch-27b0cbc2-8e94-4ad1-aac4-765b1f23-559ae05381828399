//go:build !linux

package main

// setupLED is a no-op on platforms without GPIO support (e.g. Windows,
// where the rpio dependency does not compile). The LED functions keep
// their default no-op implementations.
func setupLED(ledPin int) func() error {
	logger.Printf("LED control not supported on this platform (pin %d)", ledPin)
	return func() error {
		return nil
	}
}
