// Package pool 客户端账号绑定亲和性
package pool

import (
	"kiro-go/config"
	"time"
)

const (
	clientBindingExpiry      = 30 * time.Minute // 30分钟无请求则解绑
	tokenRefreshSkewSeconds  = 300              // token 刷新提前量（秒）
)

// cleanupExpiredBindings 定期清理过期的客户端绑定
func (p *AccountPool) cleanupExpiredBindings() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		now := time.Now()
		for clientIP, lastSeen := range p.bindingLastSeen {
			if now.Sub(lastSeen) > clientBindingExpiry {
				delete(p.clientBindings, clientIP)
				delete(p.bindingLastSeen, clientIP)
			}
		}
		p.mu.Unlock()
	}
}

// bindClient 绑定客户端到账号
func (p *AccountPool) bindClient(clientIP, accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientBindings[clientIP] = accountID
	p.bindingLastSeen[clientIP] = time.Now()
}

// unbindClient 解绑客户端
func (p *AccountPool) unbindClient(clientIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clientBindings, clientIP)
	delete(p.bindingLastSeen, clientIP)
}

// getBoundAccount 获取客户端已绑定的账号ID
func (p *AccountPool) getBoundAccount(clientIP string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.clientBindings[clientIP]
}

// updateClientActivity 更新客户端活跃时间
func (p *AccountPool) updateClientActivity(clientIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.clientBindings[clientIP]; exists {
		p.bindingLastSeen[clientIP] = time.Now()
	}
}

// tryGetBoundAccount 尝试获取已绑定的账号（不限模型）
// 返回 nil 表示账号不可用（冷却中、额度耗尽、token过期等）
func (p *AccountPool) tryGetBoundAccount(accountID, clientIP string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	// 在加权列表中查找该账号
	for i := range p.accounts {
		acc := &p.accounts[i]
		if acc.ID != accountID {
			continue
		}

		// 检查账号是否可用
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return nil
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			return nil
		}

		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	return nil
}

// tryGetBoundAccountForModel 尝试获取已绑定的账号（需支持指定模型）
// 返回 nil 表示账号不可用或不支持该模型
func (p *AccountPool) tryGetBoundAccountForModel(accountID, model, clientIP string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	// 在加权列表中查找该账号
	for i := range p.accounts {
		acc := &p.accounts[i]
		if acc.ID != accountID {
			continue
		}

		// 检查账号是否支持该模型
		if !groupPolicyAllowsModel(acc.Groups, model) {
			return nil
		}
		if !p.accountHasModel(acc.ID, model) {
			return nil
		}

		// 检查账号是否可用
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return nil
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			return nil
		}

		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	return nil
}

// tryGetBoundAccountForModelAndGroups 尝试获取已绑定的账号（需支持指定模型且符合分组策略）
// 返回 nil 表示账号不可用或不符合条件
func (p *AccountPool) tryGetBoundAccountForModelAndGroups(accountID, model string, allowedGroups []string, excludeIDs map[string]bool, clientIP string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	// 在加权列表中查找该账号
	for i := range p.accounts {
		acc := &p.accounts[i]
		if acc.ID != accountID {
			continue
		}

		// 检查是否在排除列表中
		if excludeIDs != nil && excludeIDs[acc.ID] {
			return nil
		}

		// 检查分组策略
		if len(allowedGroups) > 0 && !groupAllowed(acc.Groups, allowedGroups) {
			return nil
		}

		// 检查账号是否支持该模型
		if !groupPolicyAllowsModel(acc.Groups, model) {
			return nil
		}
		if !p.accountHasModel(acc.ID, model) {
			return nil
		}

		// 检查账号是否可用
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return nil
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			return nil
		}

		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	return nil
}
