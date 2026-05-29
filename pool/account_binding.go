// Package pool 客户端账号绑定亲和性
package pool

import (
	"kiro-go/config"
	"kiro-go/logger"
	"time"
)

const clientBindingExpiry = 30 * time.Minute // 30分钟无请求则解绑

// cleanupExpiredBindings 定期清理过期的客户端绑定
func (p *AccountPool) cleanupExpiredBindings() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		now := time.Now()
		expiredClients := make([]string, 0)
		for clientIP, lastSeen := range p.bindingLastSeen {
			if now.Sub(lastSeen) > clientBindingExpiry {
				expiredClients = append(expiredClients, clientIP)
			}
		}

		// Batch cleanup to reduce lock contention
		for _, clientIP := range expiredClients {
			// 清理反向映射
			if accountID, exists := p.clientBindings[clientIP]; exists {
				delete(p.accountBindings, accountID)
				logger.Debugf("[AccountBinding] Expired binding: client %s -> account %s", clientIP, accountID)
			}
			delete(p.clientBindings, clientIP)
			delete(p.bindingLastSeen, clientIP)
		}

		if len(expiredClients) > 0 {
			logger.Infof("[AccountBinding] Cleaned up %d expired bindings", len(expiredClients))
		}
		p.mu.Unlock()
	}
}

// bindClient 绑定客户端到账号（确保一账号一客户端）
func (p *AccountPool) bindClient(clientIP, accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 检查该账号是否已绑定到其他客户端
	if oldClient, exists := p.accountBindings[accountID]; exists && oldClient != clientIP {
		// 解绑旧客户端
		delete(p.clientBindings, oldClient)
		delete(p.bindingLastSeen, oldClient)
		logger.Debugf("[AccountBinding] Unbound old client %s from account %s", oldClient, accountID)
	}

	// 绑定新客户端
	p.clientBindings[clientIP] = accountID
	p.accountBindings[accountID] = clientIP
	p.bindingLastSeen[clientIP] = time.Now()
	logger.Debugf("[AccountBinding] Bound client %s to account %s", clientIP, accountID)
}

// unbindAccountLocked removes the client binding for an account.
// Caller must hold p.mu.
func (p *AccountPool) unbindAccountLocked(accountID string) {
	clientIP, exists := p.accountBindings[accountID]
	if !exists {
		return
	}
	delete(p.accountBindings, accountID)
	delete(p.clientBindings, clientIP)
	delete(p.bindingLastSeen, clientIP)
	logger.Debugf("[AccountBinding] Unbound client %s from unavailable account %s", clientIP, accountID)
}

// unbindClient 解绑客户端
func (p *AccountPool) unbindClient(clientIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 清理反向映射
	if accountID, exists := p.clientBindings[clientIP]; exists {
		delete(p.accountBindings, accountID)
	}

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
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-config.TokenRefreshSkewSeconds {
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
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-config.TokenRefreshSkewSeconds {
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
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-config.TokenRefreshSkewSeconds {
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
