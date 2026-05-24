// Package proxy: daily BanTime reset scheduler.
package proxy

import (
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// backgroundBanTimeResetter 每日零点清零所有账号的 BanTime。
// 停机信号复用 stopStatsSaver（同生命周期）。
func (h *Handler) backgroundBanTimeResetter() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	var lastResetDay int

	for {
		select {
		case <-h.stopStatsSaver:
			return
		case <-ticker.C:
			now := time.Now()
			currentDay := now.Day()

			// 跨日检测：当前日期与上次重置日期不同，且当前时间在 00:00-00:05 之间
			if currentDay != lastResetDay && now.Hour() == 0 && now.Minute() < 5 {
				h.resetAllBanTime()
				lastResetDay = currentDay
			}
		}
	}
}

func (h *Handler) resetAllBanTime() {
	accounts := config.GetAccounts()
	resetCount := 0

	for i := range accounts {
		if accounts[i].BanTime > 0 {
			accounts[i].BanTime = 0
			if err := config.UpdateAccount(accounts[i].ID, accounts[i]); err != nil {
				logger.Warnf("[BanTime] Reset failed for account %s: %v", accounts[i].ID, err)
			} else {
				resetCount++
			}
		}
	}

	if resetCount > 0 {
		logger.Infof("[BanTime] Daily reset completed: %d accounts cleared", resetCount)
		getBroadcaster().Publish(Event{Type: "bantime_reset", Payload: ""})
	}
}
