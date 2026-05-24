// Package proxy: alert rule evaluation engine.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// checkAlertRules 每分钟检查所有启用的告警规则
func (h *Handler) checkAlertRules() {
	rules := config.ListAlertRules()
	now := time.Now().Unix()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if rule.LastFired > 0 && now-rule.LastFired < rule.Cooldown {
			continue
		}

		triggered, value := h.evaluateRule(rule)
		if triggered {
			h.fireAlert(rule, value, now)
		}
	}
}

// evaluateRule 评估单条规则是否触发
func (h *Handler) evaluateRule(rule config.AlertRule) (bool, float64) {
	cond := rule.Condition
	store := getObserveStore()

	switch cond.Type {
	case "error_rate":
		return h.checkErrorRate(cond, store)
	case "token_rate":
		return h.checkTokenRate(cond, store)
	case "account_fail":
		return h.checkAccountFail(cond, store)
	case "model_fail":
		return h.checkModelFail(cond, store)
	case "account_banned":
		return h.checkAccountBanned(cond)
	case "quota_exhausted":
		return h.checkQuotaExhausted(cond)
	default:
		return false, 0
	}
}

// checkErrorRate 检查错误率（错误数 / 总请求数）
func (h *Handler) checkErrorRate(cond config.AlertCondition, store *observeStore) (bool, float64) {
	store.mu.RLock()
	defer store.mu.RUnlock()

	now := time.Now().Unix()
	startSlot := int((now/60 - int64(cond.Window)) % observeMinuteSlots)

	var totalReq, totalErr int64
	if cond.Target == "global" || cond.Target == "" {
		for i := 0; i < cond.Window; i++ {
			slot := (startSlot + i) % observeMinuteSlots
			totalReq += int64(store.globalRing[slot].Reqs)
			totalErr += int64(store.globalRing[slot].Failures)
		}
	} else {
		ring, ok := store.accountRings[cond.Target]
		if !ok {
			return false, 0
		}
		for i := 0; i < cond.Window; i++ {
			slot := (startSlot + i) % observeMinuteSlots
			totalReq += int64(ring[slot].Reqs)
			totalErr += int64(ring[slot].Failures)
		}
	}

	if totalReq == 0 {
		return false, 0
	}
	rate := float64(totalErr) / float64(totalReq)
	return rate >= cond.Threshold, rate
}

// checkTokenRate 检查 token 速率（tokens/min）
func (h *Handler) checkTokenRate(cond config.AlertCondition, store *observeStore) (bool, float64) {
	store.mu.RLock()
	defer store.mu.RUnlock()

	now := time.Now().Unix()
	startSlot := int((now/60 - int64(cond.Window)) % observeMinuteSlots)

	var totalTokens int64
	if cond.Target == "global" || cond.Target == "" {
		for i := 0; i < cond.Window; i++ {
			slot := (startSlot + i) % observeMinuteSlots
			totalTokens += store.globalRing[slot].InTokens + store.globalRing[slot].OutTokens
		}
	} else {
		ring, ok := store.accountRings[cond.Target]
		if !ok {
			return false, 0
		}
		for i := 0; i < cond.Window; i++ {
			slot := (startSlot + i) % observeMinuteSlots
			totalTokens += ring[slot].InTokens + ring[slot].OutTokens
		}
	}

	avgRate := float64(totalTokens) / float64(cond.Window)
	return avgRate >= cond.Threshold, avgRate
}

// checkAccountFail 检查账号连续失败次数
func (h *Handler) checkAccountFail(cond config.AlertCondition, store *observeStore) (bool, float64) {
	store.mu.RLock()
	defer store.mu.RUnlock()

	ring, ok := store.accountRings[cond.Target]
	if !ok {
		return false, 0
	}

	now := time.Now().Unix()
	currentSlot := int((now / 60) % observeMinuteSlots)

	var consecutiveFails int64
	for i := 0; i < cond.Window && i < observeMinuteSlots; i++ {
		slot := (currentSlot - i + observeMinuteSlots) % observeMinuteSlots
		if ring[slot].Failures > 0 && ring[slot].Reqs == ring[slot].Failures {
			consecutiveFails++
		} else if ring[slot].Reqs > 0 {
			break
		}
	}

	return float64(consecutiveFails) >= cond.Threshold, float64(consecutiveFails)
}

// checkModelFail 检查模型失败率（注：modelStat 无 errors 字段，此检测器暂不可用）
func (h *Handler) checkModelFail(cond config.AlertCondition, store *observeStore) (bool, float64) {
	store.mu.RLock()
	defer store.mu.RUnlock()

	stat, ok := store.modelStats[cond.Target]
	if !ok {
		return false, 0
	}

	if stat.Reqs == 0 {
		return false, 0
	}
	// modelStat 当前无 errors 字段，无法计算失败率
	// 需要在 observe.go 中补充 modelStat.Failures 字段
	return false, 0
}

// checkAccountBanned 检查账号封禁状态
func (h *Handler) checkAccountBanned(cond config.AlertCondition) (bool, float64) {
	accounts := config.GetAccounts()
	var bannedCount float64
	for _, acc := range accounts {
		if cond.Target == "global" || cond.Target == "" || cond.Target == acc.ID {
			if acc.BanStatus == "BANNED" || acc.BanStatus == "SUSPENDED" {
				bannedCount++
				if cond.Target != "global" && cond.Target != "" {
					return true, 1.0
				}
			}
		}
	}
	if cond.Target == "global" || cond.Target == "" {
		return bannedCount >= cond.Threshold, bannedCount
	}
	return false, 0
}

// checkQuotaExhausted 检查账号额度耗尽
func (h *Handler) checkQuotaExhausted(cond config.AlertCondition) (bool, float64) {
	accounts := config.GetAccounts()
	var exhaustedCount float64
	for _, acc := range accounts {
		if cond.Target == "global" || cond.Target == "" || cond.Target == acc.ID {
			usagePercent := acc.UsagePercent
			if acc.TrialStatus == "ACTIVE" && acc.TrialUsagePercent > usagePercent {
				usagePercent = acc.TrialUsagePercent
			}
			if usagePercent >= cond.Threshold {
				exhaustedCount++
				if cond.Target != "global" && cond.Target != "" {
					return true, usagePercent
				}
			}
		}
	}
	if cond.Target == "global" || cond.Target == "" {
		return exhaustedCount >= cond.Threshold, exhaustedCount
	}
	return false, 0
}

// fireAlert 触发告警
func (h *Handler) fireAlert(rule config.AlertRule, value float64, firedAt int64) {
	condDesc := fmt.Sprintf("%s %s >= %.2f (actual: %.2f)",
		rule.Condition.Type, rule.Condition.Target, rule.Condition.Threshold, value)

	logger.Warnf("[Alert] Rule '%s' triggered: %s", rule.Name, condDesc)

	var executedActions []string
	for _, action := range rule.Actions {
		switch action.Type {
		case "webhook":
			if err := h.executeWebhook(action, rule, value); err != nil {
				logger.Warnf("[Alert] Webhook failed: %v", err)
			} else {
				executedActions = append(executedActions, "webhook:"+action.Config["url"])
			}
		case "logsse":
			getBroadcaster().Publish(Event{
				Type:    "alert_fired",
				Payload: rule.ID,
			})
			executedActions = append(executedActions, "logsse")
		}
	}

	config.RecordAlertFired(rule.ID, rule.Name, condDesc, value, executedActions)
	config.MarkRuleFired(rule.ID, firedAt)
}

// executeWebhook 执行 webhook 通知
func (h *Handler) executeWebhook(action config.AlertAction, rule config.AlertRule, value float64) error {
	url := action.Config["url"]
	if url == "" {
		return fmt.Errorf("webhook url is empty")
	}

	payload := map[string]interface{}{
		"rule_id":    rule.ID,
		"rule_name":  rule.Name,
		"condition":  rule.Condition,
		"value":      value,
		"threshold":  rule.Condition.Threshold,
		"fired_at":   time.Now().Unix(),
		"alert_type": "kiro-go-alert",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
