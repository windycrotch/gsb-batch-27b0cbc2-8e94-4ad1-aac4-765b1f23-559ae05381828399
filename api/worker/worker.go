package worker

import (
	"context"
	"os"
	"os/signal"
	"sync"
)

// Worker variables.
var (
	AlertMessageFormat = `
*Encode Successful!* :tada:\n
"*Job ID*: %s:\n"
"*Preset*: %s\n"
"*Source*: %s\n"
"*Destination*: %s\n\n"
`

	shutdownOnce   sync.Once
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
)

// jobContext returns a context that is cancelled when the worker process
// receives an interrupt. Every job execution derives its own cancellable
// context from it.
func jobContext() context.Context {
	shutdownOnce.Do(func() {
		shutdownCtx, shutdownCancel = context.WithCancel(context.Background())
		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, os.Interrupt, os.Kill)
		go func() {
			<-signalChan
			shutdownCancel()
		}()
	})
	return shutdownCtx
}
