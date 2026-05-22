// Package proxy: background alert rule checker.
package proxy

import (
	"time"
)

// backgroundAlertChecker 每分钟检查所有启用的告警规则。
// 停机信号复用 stopStatsSaver（同生命周期）。
func (h *Handler) backgroundAlertChecker() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopStatsSaver:
			return
		case <-ticker.C:
			h.checkAlertRules()
		}
	}
}
