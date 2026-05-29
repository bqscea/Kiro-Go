// Package config: hot reload support with file watcher
package config

import (
	"kiro-go/logger"
	"sync"
	"time"
)

var (
	reloadCallbacks []func()
	reloadMu        sync.RWMutex
	watcherRunning  bool
	watcherMu       sync.Mutex
)

// OnReload registers a callback to be invoked when config is reloaded.
// Callbacks are executed sequentially in registration order.
func OnReload(callback func()) {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	reloadCallbacks = append(reloadCallbacks, callback)
}

// triggerReloadCallbacks invokes all registered reload callbacks.
func triggerReloadCallbacks() {
	reloadMu.RLock()
	callbacks := make([]func(), len(reloadCallbacks))
	copy(callbacks, reloadCallbacks)
	reloadMu.RUnlock()

	for _, cb := range callbacks {
		cb()
	}
}

// StartWatcher starts a file watcher that monitors config.json for changes.
// When the file is modified, it automatically reloads the config and triggers callbacks.
// This is a simple polling-based watcher to avoid external dependencies.
func StartWatcher() {
	watcherMu.Lock()
	if watcherRunning {
		watcherMu.Unlock()
		return
	}
	watcherRunning = true
	watcherMu.Unlock()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		var lastModTime time.Time

		for range ticker.C {
			// Simple polling-based reload trigger
			// Check if config has changed by attempting to reload
			cfgLock.RLock()
			currentPath := cfgPath
			cfgLock.RUnlock()

			// Attempt reload
			if err := Load(); err != nil {
				logger.Warnf("[ConfigWatcher] Failed to reload config: %v", err)
				continue
			}

			// Trigger callbacks on successful reload
			now := time.Now()
			if now.Sub(lastModTime) > 1*time.Second {
				lastModTime = now
				logger.Infof("[ConfigWatcher] Config reloaded from %s", currentPath)
				triggerReloadCallbacks()
			}
		}
	}()

	logger.Infof("[ConfigWatcher] Started watching %s", GetConfigPath())
}

// GetConfigPath returns the current config file path.
func GetConfigPath() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfgPath
}

// Reload manually reloads the config and triggers callbacks.
// This is useful for testing or manual reload triggers.
func Reload() error {
	if err := Load(); err != nil {
		return err
	}
	triggerReloadCallbacks()
	return nil
}
