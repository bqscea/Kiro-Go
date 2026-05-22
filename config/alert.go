// Package config: alert rules and history management.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// AlertRule 告警规则
type AlertRule struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Condition AlertCondition `json:"condition"`
	Actions   []AlertAction  `json:"actions"`
	Cooldown  int64          `json:"cooldown"`    // 秒
	LastFired int64          `json:"last_fired,omitempty"`
}

// AlertCondition 触发条件
type AlertCondition struct {
	Type      string  `json:"type"`      // error_rate, token_rate, account_fail, model_fail
	Threshold float64 `json:"threshold"` // 阈值
	Window    int     `json:"window"`    // 时间窗口（分钟）
	Target    string  `json:"target"`    // account_id / model / "global"
}

// AlertAction 告警动作
type AlertAction struct {
	Type   string            `json:"type"`   // webhook, logsse
	Config map[string]string `json:"config"` // webhook: url, method; logsse: level
}

// AlertHistory 告警历史记录
type AlertHistory struct {
	ID        string   `json:"id"`
	RuleID    string   `json:"rule_id"`
	RuleName  string   `json:"rule_name"`
	FiredAt   int64    `json:"fired_at"`
	Condition string   `json:"condition"` // 触发条件描述
	Value     float64  `json:"value"`     // 实际值
	Actions   []string `json:"actions"`   // 执行的动作
}

// AlertConfig 告警配置（持久化到 config.json）
type AlertConfig struct {
	Rules []AlertRule `json:"rules,omitempty"`
}

var (
	alertHistoryMu sync.RWMutex
	alertHistory   []AlertHistory // 最新的在末尾，max 100
)

func init() {
	alertHistory = make([]AlertHistory, 0, 100)
}

// CreateAlertRule 创建告警规则
func CreateAlertRule(rule AlertRule) (AlertRule, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	if rule.ID == "" {
		rule.ID = genAlertID()
	}
	if rule.Name == "" {
		return AlertRule{}, fmt.Errorf("rule name is required")
	}
	if rule.Cooldown <= 0 {
		rule.Cooldown = 300 // 默认5分钟
	}

	cfg.Alert.Rules = append(cfg.Alert.Rules, rule)
	if err := Save(); err != nil {
		cfg.Alert.Rules = cfg.Alert.Rules[:len(cfg.Alert.Rules)-1]
		return AlertRule{}, err
	}
	return rule, nil
}

// ListAlertRules 列出所有规则
func ListAlertRules() []AlertRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	rules := make([]AlertRule, len(cfg.Alert.Rules))
	copy(rules, cfg.Alert.Rules)
	return rules
}

// FindAlertRule 查找规则
func FindAlertRule(id string) (AlertRule, error) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, r := range cfg.Alert.Rules {
		if r.ID == id {
			return r, nil
		}
	}
	return AlertRule{}, fmt.Errorf("rule not found: %s", id)
}

// UpdateAlertRule 更新规则
func UpdateAlertRule(id string, updated AlertRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, r := range cfg.Alert.Rules {
		if r.ID == id {
			updated.ID = id
			cfg.Alert.Rules[i] = updated
			return Save()
		}
	}
	return fmt.Errorf("rule not found: %s", id)
}

// DeleteAlertRule 删除规则
func DeleteAlertRule(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, r := range cfg.Alert.Rules {
		if r.ID == id {
			cfg.Alert.Rules = append(cfg.Alert.Rules[:i], cfg.Alert.Rules[i+1:]...)
			return Save()
		}
	}
	return fmt.Errorf("rule not found: %s", id)
}

// RecordAlertFired 记录告警触发
func RecordAlertFired(ruleID, ruleName, condition string, value float64, actions []string) {
	alertHistoryMu.Lock()
	defer alertHistoryMu.Unlock()

	entry := AlertHistory{
		ID:        genAlertID(),
		RuleID:    ruleID,
		RuleName:  ruleName,
		FiredAt:   time.Now().Unix(),
		Condition: condition,
		Value:     value,
		Actions:   actions,
	}

	alertHistory = append(alertHistory, entry)
	if len(alertHistory) > 100 {
		alertHistory = alertHistory[1:]
	}
}

// ListAlertHistory 列出告警历史（最近优先）
func ListAlertHistory(limit int) []AlertHistory {
	alertHistoryMu.RLock()
	defer alertHistoryMu.RUnlock()

	n := len(alertHistory)
	if limit <= 0 || limit > n {
		limit = n
	}

	result := make([]AlertHistory, limit)
	for i := 0; i < limit; i++ {
		result[i] = alertHistory[n-1-i]
	}
	return result
}

// MarkRuleFired 更新规则的 LastFired 时间
func MarkRuleFired(ruleID string, firedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, r := range cfg.Alert.Rules {
		if r.ID == ruleID {
			cfg.Alert.Rules[i].LastFired = firedAt
			return Save()
		}
	}
	return fmt.Errorf("rule not found: %s", ruleID)
}

func genAlertID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
