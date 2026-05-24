// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"kiro-go/config"
	"kiro-go/logger"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const overageFrequencyScale = 10
const tokenRefreshSkewSeconds int64 = 120

// 账号预占锁时长（防止并发请求选中同一账号）
const accountReservationDuration = 5 * time.Second

// AccountPool 账号池
type AccountPool struct {
	mu              sync.RWMutex
	accounts        []config.Account
	totalAccounts   int
	currentIndex    uint64
	cooldowns       map[string]time.Time       // 账号冷却时间
	errorCounts     map[string]int             // 连续错误计数
	modelLists      map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	clientBindings  map[string]string          // clientIP:Port → accountID（会话级绑定）
	bindingLastSeen map[string]time.Time       // clientIP:Port → 最后请求时间（30分钟无请求则解绑）
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:       make(map[string]time.Time),
			errorCounts:     make(map[string]int),
			modelLists:      make(map[string]map[string]bool),
			clientBindings:  make(map[string]string),
			bindingLastSeen: make(map[string]time.Time),
		}
		pool.Reload()
		// 加载持久化的冷却状态
		if err := pool.loadCooldowns(); err != nil {
			// 加载失败不影响启动，仅记录日志
			_ = err
		}
		// 启动绑定过期清理协程
		go pool.cleanupExpiredBindings()
	})
	return pool
}

// Reload 从配置重新加载账号
// 构建加权列表：weight<=1 出现 1 次，weight>=2 出现 weight 次
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := config.GetEnabledAccounts()
	var weighted []config.Account
	for _, a := range enabled {
		w := effectiveWeight(a.Weight) * overageFrequencyScale
		if isOverUsageLimit(a) {
			if !a.AllowOverage {
				continue
			}
			w = effectiveOverageWeight(a.OverageWeight)
		}
		for j := 0; j < w; j++ {
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
	p.totalAccounts = len(enabled)
	logger.Infof("[Pool] Loaded %d enabled accounts (%d weighted slots)", len(enabled), len(weighted))
}

// GetNext 获取下一个可用账号（根据 LoadBalancingMode 选择策略）
// clientIP 为客户端标识（IP:Port），用于会话级账号绑定
func (p *AccountPool) GetNext(clientIP string) *config.Account {
	// 优先返回已绑定的账号
	if clientIP != "" {
		if boundAccountID := p.getBoundAccount(clientIP); boundAccountID != "" {
			if acc := p.tryGetBoundAccount(boundAccountID, clientIP); acc != nil {
				p.updateClientActivity(clientIP)
				return acc
			}
			// 绑定的账号不可用，解绑
			p.unbindClient(clientIP)
		}
	}

	mode := config.GetLoadBalancingMode()
	var acc *config.Account
	if mode == "priority" {
		acc = p.getNextPriority()
	} else {
		acc = p.getNextBalanced()
	}

	// 绑定新账号到客户端
	if acc != nil && clientIP != "" {
		p.bindClient(clientIP, acc.ID)
	}
	return acc
}

// getNextPriority 按优先级顺序选择账号（Weight 越大优先级越高）
func (p *AccountPool) getNextPriority() *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	// 去重并按优先级排序
	uniqueAccounts := p.getUniqueSortedAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	// 按优先级顺序尝试
	for _, acc := range uniqueAccounts {
		// 跳过冷却中的账号
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}

		// 跳过即将过期的 Token
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}

		// 跳过额度已用尽的账号（账号级 AllowOverage 或全局 AllowOverUsage 可放行）
		if isOverUsageLimit(acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}

		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return &acc
	}

	// 无可用账号，返回冷却时间最短的（排除额度用尽的，除非允许超额）
	var best *config.Account
	var earliest time.Time
	for i := range uniqueAccounts {
		acc := &uniqueAccounts[i]
		// 额度用尽的账号不作为 fallback（除非账号级或全局允许超额）
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	if best != nil {
		p.mu.RUnlock()
		p.markInUse(best.ID)
		p.mu.RLock()
	}
	return best
}

// getNextBalanced 加权轮询选择账号（原 GetNext 逻辑）
func (p *AccountPool) getNextBalanced() *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	// 加权轮询查找可用账号
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if seen[acc.ID] {
			continue
		}

		// 跳过冷却中的账号
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}

		// 跳过即将过期的 Token
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			seen[acc.ID] = true
			continue
		}

		// 跳过额度已用尽的账号（账号级 AllowOverage 或全局 AllowOverUsage 可放行）
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			seen[acc.ID] = true
			continue
		}

		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	// 无可用账号，返回冷却时间最短的（排除额度用尽的，除非允许超额）
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		// 额度用尽的账号不作为 fallback（除非账号级或全局允许超额）
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

// getUniqueSortedAccounts 去重并按优先级排序（Weight 越大优先级越高）
func (p *AccountPool) getUniqueSortedAccounts() []config.Account {
	seen := make(map[string]bool)
	var unique []config.Account
	for _, acc := range p.accounts {
		if !seen[acc.ID] {
			seen[acc.ID] = true
			unique = append(unique, acc)
		}
	}
	sort.Slice(unique, func(i, j int) bool {
		wi := effectiveWeight(unique[i].Weight)
		wj := effectiveWeight(unique[j].Weight)
		if isOverUsageLimit(unique[i]) && unique[i].AllowOverage {
			wi = effectiveOverageWeight(unique[i].OverageWeight)
		}
		if isOverUsageLimit(unique[j]) && unique[j].AllowOverage {
			wj = effectiveOverageWeight(unique[j].OverageWeight)
		}
		return wi > wj // 大权重 = 高优先级
	})
	return unique
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// clientIP 为客户端标识（IP:Port），用于会话级账号绑定。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string, clientIP string) *config.Account {
	// 优先返回已绑定的账号（如果支持该模型）
	if clientIP != "" {
		if boundAccountID := p.getBoundAccount(clientIP); boundAccountID != "" {
			if acc := p.tryGetBoundAccountForModel(boundAccountID, model, clientIP); acc != nil {
				p.updateClientActivity(clientIP)
				return acc
			}
			// 绑定的账号不可用或不支持该模型，解绑
			p.unbindClient(clientIP)
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	if config.GetLoadBalancingMode() == "priority" {
		return p.getNextPriorityForModel(model)
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if seen[acc.ID] {
			continue
		}
		if !groupPolicyAllowsModel(acc.Groups, model) {
			seen[acc.ID] = true
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			seen[acc.ID] = true
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			seen[acc.ID] = true
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			seen[acc.ID] = true
			continue
		}
		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	// fallback：找冷却时间最短且支持该模型的账号
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			p.mu.RUnlock()
			p.markInUse(acc.ID)
			// 绑定新账号到客户端
			if clientIP != "" {
				p.bindClient(clientIP, acc.ID)
			}
			p.mu.RLock()
			return acc
		}
	}
	if best != nil {
		p.mu.RUnlock()
		p.markInUse(best.ID)
		// 绑定新账号到客户端
		if clientIP != "" {
			p.bindClient(clientIP, best.ID)
		}
		p.mu.RLock()
	}
	return best
}

// getNextPriorityForModel 按优先级顺序选择支持指定模型的账号（Weight 越大优先级越高）。
// 调用方必须持有 p.mu.RLock。
func (p *AccountPool) getNextPriorityForModel(model string) *config.Account {
	uniqueAccounts := p.getUniqueSortedAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	for _, acc := range uniqueAccounts {
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isOverUsageLimit(acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return &acc
	}

	// fallback：找冷却时间最短且支持该模型的账号
	var best *config.Account
	var earliest time.Time
	for i := range uniqueAccounts {
		acc := &uniqueAccounts[i]
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = &uniqueAccounts[i]
				earliest = cooldown
			}
		} else {
			p.mu.RUnlock()
			p.markInUse(acc.ID)
			p.mu.RLock()
			return &uniqueAccounts[i]
		}
	}
	if best != nil {
		p.mu.RUnlock()
		p.markInUse(best.ID)
		p.mu.RLock()
	}
	return best
}

// getNextPriorityForModelAndGroups 按优先级顺序选择支持指定模型且属于允许分组的账号。
// 调用方必须持有 p.mu.RLock。
func (p *AccountPool) getNextPriorityForModelAndGroups(model string, allowedGroups []string, excludeIDs map[string]bool) *config.Account {
	uniqueAccounts := p.getUniqueSortedAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	for _, acc := range uniqueAccounts {
		if excludeIDs != nil && excludeIDs[acc.ID] {
			continue
		}
		if len(allowedGroups) > 0 && !groupAllowed(acc.Groups, allowedGroups) {
			continue
		}
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isOverUsageLimit(acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return &acc
	}

	// fallback：找冷却时间最短且支持该模型 + group 的账号
	var best *config.Account
	var earliest time.Time
	for i := range uniqueAccounts {
		acc := &uniqueAccounts[i]
		if excludeIDs != nil && excludeIDs[acc.ID] {
			continue
		}
		if len(allowedGroups) > 0 && !groupAllowed(acc.Groups, allowedGroups) {
			continue
		}
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = &uniqueAccounts[i]
				earliest = cooldown
			}
		} else {
			p.mu.RUnlock()
			p.markInUse(acc.ID)
			p.mu.RLock()
			return &uniqueAccounts[i]
		}
	}
	if best != nil {
		p.mu.RUnlock()
		p.markInUse(best.ID)
		p.mu.RLock()
	}
	return best
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// markInUse 标记账号为使用中（设置短期cooldown防止并发请求重复选中）
func (p *AccountPool) markInUse(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldowns[id] = time.Now().Add(accountReservationDuration)
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) int {
	p.mu.Lock()
	p.errorCounts[id]++
	count := p.errorCounts[id]

	if isQuotaError {
		// 配额错误，冷却至重置日期
		cooldownUntil := p.calculateQuotaCooldown(id)
		p.cooldowns[id] = cooldownUntil
	} else if count >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
	p.mu.Unlock()

	// 异步保存冷却状态（避免阻塞请求路径）
	go func() {
		if err := p.SaveCooldowns(); err != nil {
			// 保存失败不影响运行，仅记录日志
			_ = err
		}
	}()

	return count
}

// calculateQuotaCooldown 计算配额耗尽的冷却时间（至重置日期）
func (p *AccountPool) calculateQuotaCooldown(accountID string) time.Time {
	// 查找账号的重置日期
	var resetDate string
	for _, acc := range p.accounts {
		if acc.ID == accountID {
			resetDate = acc.NextResetDate
			break
		}
	}

	// 解析重置日期（格式：YYYY-MM-DD）
	if resetDate != "" {
		if t, err := time.Parse("2006-01-02", resetDate); err == nil {
			// 重置日期 + 1 天（确保跨过重置点）
			resetTime := t.Add(24 * time.Hour)
			if resetTime.After(time.Now()) {
				return resetTime
			}
		}
	}

	// 兜底：重置日期无效或已过期，冷却 24 小时
	return time.Now().Add(24 * time.Hour)
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

// GroupStat 单个分组的运行时状态。
type GroupStat struct {
	Group     string `json:"group"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Cooldown  int    `json:"cooldown"`
	Disabled  int    `json:"disabled"`
}

// GroupStats 按分组聚合账号池状态。
// 包含禁用账号（通过 config.GetAccounts），便于运维查看整体健康。
func (p *AccountPool) GroupStats() []GroupStat {
	p.mu.RLock()
	cooldowns := make(map[string]time.Time, len(p.cooldowns))
	for k, v := range p.cooldowns {
		cooldowns[k] = v
	}
	p.mu.RUnlock()

	all := config.GetAccounts()
	now := time.Now()
	type bucket struct {
		total, avail, cooldown, disabled int
	}
	groups := make(map[string]*bucket)
	for _, a := range all {
		accountGroups := a.Groups
		if len(accountGroups) == 0 {
			accountGroups = []string{"default"}
		}
		for _, g := range accountGroups {
			g = strings.TrimSpace(g)
			if g == "" {
				g = "default"
			}
			b, ok := groups[g]
			if !ok {
				b = &bucket{}
				groups[g] = b
			}
			b.total++
			if !a.Enabled {
				b.disabled++
				continue
			}
			if cd, ok := cooldowns[a.ID]; ok && now.Before(cd) {
				b.cooldown++
				continue
			}
			if a.ExpiresAt > 0 && now.Unix() > a.ExpiresAt-tokenRefreshSkewSeconds {
				b.cooldown++
				continue
			}
			b.avail++
		}
	}

	out := make([]GroupStat, 0, len(groups))
	for g, b := range groups {
		out = append(out, GroupStat{Group: g, Total: b.total, Available: b.avail, Cooldown: b.cooldown, Disabled: b.disabled})
	}
	// 稳定排序：default 优先，其余按字母升序
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group == "default" {
			return true
		}
		if out[j].Group == "default" {
			return false
		}
		return out[i].Group < out[j].Group
	})
	return out
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// normalizeGroup treats empty as the implicit "default" group.
func normalizeGroup(g string) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return "default"
	}
	return g
}

// groupAllowed reports whether any of the account's groups is permitted by the
// allowedGroups whitelist. nil/empty whitelist or a "*" entry means any group.
func groupAllowed(accountGroups []string, allowedGroups []string) bool {
	if len(allowedGroups) == 0 {
		return true
	}
	// Empty account groups treated as implicit "default" group
	if len(accountGroups) == 0 {
		accountGroups = []string{""}
	}
	for _, ag := range accountGroups {
		normalized := normalizeGroup(ag)
		for _, g := range allowedGroups {
			gg := strings.TrimSpace(g)
			if gg == "*" {
				return true
			}
			if normalizeGroup(gg) == normalized {
				return true
			}
		}
	}
	return false
}

// groupPolicyAllowsModel 在 GetNextForModelAndGroups 路由时，校验账号 Groups 的策略
// 是否允许该模型。空 groups 视为 "default"。无策略 = 不限制。
// 只要任一分组允许该模型即可。
func groupPolicyAllowsModel(accountGroups []string, model string) bool {
	if len(accountGroups) == 0 {
		return config.GroupAllowsModel("", model)
	}
	for _, g := range accountGroups {
		if config.GroupAllowsModel(normalizeGroup(g), model) {
			return true
		}
	}
	return false
}

// GetNextForModelAndGroups picks the next available account that supports the model
// and whose group is permitted by allowedGroups. allowedGroups nil/empty = any group.
// clientIP 为客户端标识（IP:Port），用于会话级账号绑定。
func (p *AccountPool) GetNextForModelAndGroups(model string, allowedGroups []string, clientIP string) *config.Account {
	return p.GetNextForModelAndGroupsExcluding(model, allowedGroups, nil, clientIP)
}

// GetNextForModelAndGroupsExcluding picks the next available account, excluding specified IDs.
// Used by retry logic to skip already-failed accounts.
// clientIP 为客户端标识（IP:Port），用于会话级账号绑定。
func (p *AccountPool) GetNextForModelAndGroupsExcluding(model string, allowedGroups []string, excludeIDs map[string]bool, clientIP string) *config.Account {
	if len(allowedGroups) == 0 && excludeIDs == nil {
		return p.GetNextForModel(model, clientIP)
	}

	// 优先返回已绑定的账号（如果支持该模型且符合分组策略）
	if clientIP != "" {
		if boundAccountID := p.getBoundAccount(clientIP); boundAccountID != "" {
			if acc := p.tryGetBoundAccountForModelAndGroups(boundAccountID, model, allowedGroups, excludeIDs, clientIP); acc != nil {
				p.updateClientActivity(clientIP)
				return acc
			}
			// 绑定的账号不可用或不符合条件，解绑
			p.unbindClient(clientIP)
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	if config.GetLoadBalancingMode() == "priority" {
		return p.getNextPriorityForModelAndGroups(model, allowedGroups, excludeIDs)
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if seen[acc.ID] {
			continue
		}
		if excludeIDs != nil && excludeIDs[acc.ID] {
			seen[acc.ID] = true
			continue
		}
		if len(allowedGroups) > 0 && !groupAllowed(acc.Groups, allowedGroups) {
			seen[acc.ID] = true
			continue
		}
		if !groupPolicyAllowsModel(acc.Groups, model) {
			seen[acc.ID] = true
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			seen[acc.ID] = true
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}
		if acc.ExpiresAt > 0 && time.Now().Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			seen[acc.ID] = true
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			seen[acc.ID] = true
			continue
		}
		p.mu.RUnlock()
		p.markInUse(acc.ID)
		p.mu.RLock()
		return acc
	}

	// fallback：找冷却时间最短且支持该模型 + group 的账号
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excludeIDs != nil && excludeIDs[acc.ID] {
			continue
		}
		if len(allowedGroups) > 0 && !groupAllowed(acc.Groups, allowedGroups) {
			continue
		}
		if !groupPolicyAllowsModel(acc.Groups, model) {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isOverUsageLimit(*acc) && !acc.AllowOverage && !allowOverUsage {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			p.mu.RUnlock()
			p.markInUse(acc.ID)
			// 绑定新账号到客户端
			if clientIP != "" {
				p.bindClient(clientIP, acc.ID)
			}
			p.mu.RLock()
			return acc
		}
	}
	if best != nil {
		p.mu.RUnlock()
		p.markInUse(best.ID)
		// 绑定新账号到客户端
		if clientIP != "" {
			p.bindClient(clientIP, best.ID)
		}
		p.mu.RLock()
	}
	return best
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

func effectiveOverageWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	if weight > overageFrequencyScale {
		return overageFrequencyScale
	}
	return weight
}
