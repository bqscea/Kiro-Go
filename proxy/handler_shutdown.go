// Package proxy: graceful shutdown for Handler.
package proxy

import (
	"kiro-go/logger"
)

// Shutdown gracefully stops all background goroutines and persists state.
// Safe to call multiple times (idempotent via sync.Once).
func (h *Handler) Shutdown() {
	h.shutdownOnce.Do(func() {
		logger.Infof("[Shutdown] Stopping background goroutines...")

		// Signal all background goroutines to stop
		close(h.stopRefresh)
		close(h.stopStatsSaver)

		// Wait briefly for goroutines to finish their cleanup
		// (each goroutine saves state on exit via defer or explicit save)
		// No WaitGroup needed since each goroutine handles its own cleanup

		logger.Infof("[Shutdown] All background tasks stopped")
	})
}
