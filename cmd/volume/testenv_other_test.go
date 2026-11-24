//go:build windows || darwin

package main

// useIsolatedFakePactl is only meaningful on Linux, where the testdata
// fake pactl backend exists. Other platforms have nothing to isolate.
func useIsolatedFakePactl() {}
