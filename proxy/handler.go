package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	promptCache     *promptCacheTracker
	tokenRefreshMu  sync.Mutex
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:     make(chan struct{}),
		stopStatsSaver:  make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
	}
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	// 启动后台 observe tick (每 5s 推一次)
	go h.backgroundObserveTick()
	// 启动后台 observe 数据保存 (每 5min 保存一次)
	go h.backgroundObserveSaver()
	// 启动时加载观测数据
	if err := getObserveStore().Load(); err != nil {
		logger.Warnf("[Observe] Failed to load: %v", err)
	}
	// 启动后台定时快照 (每 5min 检查)
	go h.backgroundBackupScheduler()
	// 启动后台告警检测 (每 1min 检查)
	go h.backgroundAlertChecker()
	// 启动后台冷却状态保存 (每 5min 保存一次)
	go h.backgroundCooldownSaver()
	return h
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	now := time.Now().Unix()
	const refreshInterval = 30 * 60 // 30 分钟

	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 跳过被封禁或静默的账号，避免浪费 API 调用
		if account.Silent || (account.BanStatus != "" && account.BanStatus != "ACTIVE") {
			continue
		}

		// 检查 token 是否需要刷新
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
			newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
				// 不跳过，继续尝试刷新账户信息（可能会失败，但至少尝试）
			} else {
				account.AccessToken = newAccessToken
				if newRefreshToken != "" {
					account.RefreshToken = newRefreshToken
				}
				account.ExpiresAt = newExpiresAt
				config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				if profileArn != "" {
					account.ProfileArn = profileArn
					config.UpdateAccountProfileArn(account.ID, profileArn)
				}
			}
		}

		// 检查账户信息是否需要刷新（基于 LastRefresh 到期时间）
		if account.LastRefresh > 0 && now-account.LastRefresh < refreshInterval {
			continue
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// extractProvidedKey 从请求头读取调用方提交的 API Key（Authorization Bearer 或 X-Api-Key）
func extractProvidedKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	apiKeyHeader := r.Header.Get("X-Api-Key")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	return apiKeyHeader
}

// apiKeyCtxKey 用于在 request context 中存储命中的 ApiKeyEntry。
type apiKeyCtxKey struct{}

// matchedApiKeyEntry 从 context 中取出 handler 阶段命中的多 Key 表条目。
// 旧版根 ApiKey 命中或未启用鉴权时返回 nil。
func matchedApiKeyEntry(r *http.Request) *config.ApiKeyEntry {
	if r == nil {
		return nil
	}
	if v := r.Context().Value(apiKeyCtxKey{}); v != nil {
		if e, ok := v.(*config.ApiKeyEntry); ok {
			return e
		}
	}
	return nil
}

// validateApiKey 验证 API Key（仅校验，不返回 group 限制）。
// 命中多 Key 表时把 entry 写入 r.Context，便于下游做限流 / 统计。
func (h *Handler) validateApiKey(r *http.Request) bool {
	if !config.IsApiKeyRequired() {
		return true
	}

	provided := extractProvidedKey(r)

	// 多 Key 表优先匹配
	if entry := config.FindApiKeyEntry(provided); entry != nil {
		ctx := context.WithValue(r.Context(), apiKeyCtxKey{}, entry)
		*r = *r.WithContext(ctx)
		return true
	}

	// 兼容旧版：根 ApiKey
	expectedKey := config.GetApiKey()
	if expectedKey == "" {
		// 既无多 Key 表命中，也无根 Key 配置 → 视为未启用鉴权
		if len(config.GetApiKeys()) == 0 {
			return true
		}
		return false
	}
	return config.SecureCompareString(provided, expectedKey)
}

// enforceRateLimit 在 ServeHTTP 路由分发时对命中多 Key 表的请求做 RPM/RPD 限流。
// 返回 (允许, 拒绝原因)；旧版根 ApiKey / 未启用鉴权 一律放行。
func (h *Handler) enforceRateLimit(r *http.Request) (bool, string) {
	entry := matchedApiKeyEntry(r)
	if entry == nil {
		return true, ""
	}
	return getApiKeyLimiter().Allow(entry.ID, entry.RPM, entry.RPD)
}

// resolveAllowedGroups 返回当前请求允许使用的账号分组白名单。
// 返回 nil 表示不限制（旧版 ApiKey 或未启用鉴权）。
func (h *Handler) resolveAllowedGroups(r *http.Request) []string {
	if !config.IsApiKeyRequired() {
		return nil
	}
	provided := extractProvidedKey(r)
	if entry := config.FindApiKeyEntry(provided); entry != nil {
		// 空 groups 或包含 "*" → 不限制
		if len(entry.Groups) == 0 {
			return nil
		}
		for _, g := range entry.Groups {
			if strings.TrimSpace(g) == "*" {
				return nil
			}
		}
		return entry.Groups
	}
	// 命中根 ApiKey → 不限制
	return nil
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		if !h.validateApiKey(r) {
			h.sendClaudeError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if ok, reason := h.enforceRateLimit(r); !ok {
			h.sendClaudeError(w, 429, "rate_limit_error", reason)
			return
		}
		h.handleClaudeMessages(w, r)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		if !h.validateApiKey(r) {
			h.sendClaudeError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if ok, reason := h.enforceRateLimit(r); !ok {
			h.sendClaudeError(w, 429, "rate_limit_error", reason)
			return
		}
		h.handleCountTokens(w, r)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		if !h.validateApiKey(r) {
			h.sendOpenAIError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if ok, reason := h.enforceRateLimit(r); !ok {
			h.sendOpenAIError(w, 429, "rate_limit_exceeded", reason)
			return
		}
		h.handleOpenAIChat(w, r)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.refreshModelsCache()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// 添加别名模型
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)

	// 先做一次鉴权，把命中的 ApiKey entry 写入 ctx；
	// 用于按 keyID 过滤「按 Key 绑定」的模型别名。
	// 即使是旧版根 ApiKey 命中或未启用鉴权，aliasKeyID 仍为空 → 仅展开全局别名。
	var aliasKeyID string
	if config.IsApiKeyRequired() {
		_ = h.validateApiKey(r)
		if entry := matchedApiKeyEntry(r); entry != nil {
			aliasKeyID = entry.ID
		}
	}

	// 暴露用户配置的模型别名（owned_by=alias，避免与真实模型冲突）
	// 仅暴露：全局别名（KeyIDs 为空）；或绑定到当前命中 Key 的别名。
	for _, alias := range config.GetModelAliases() {
		if !alias.Enabled || strings.TrimSpace(alias.From) == "" {
			continue
		}
		if len(alias.KeyIDs) > 0 {
			if aliasKeyID == "" {
				continue
			}
			matched := false
			for _, id := range alias.KeyIDs {
				if strings.TrimSpace(id) == aliasKeyID {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		models = append(models, buildModelInfo(alias.From, "alias", true))
	}

	// 按 API Key 允许的 group + group policy 缩水模型列表
	if config.IsApiKeyRequired() {
		allowedGroups := h.resolveAllowedGroups(r)
		models = filterModelsByPolicies(models, allowedGroups, thinkingSuffix)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

// filterModelsByPolicies 把模型列表按「Key 允许的 group」覆盖到的所有 group policy 求并集后过滤。
// allowedGroups nil/空 → 取全部已配置策略的并集 + 默认无策略组（不限）。
func filterModelsByPolicies(models []map[string]interface{}, allowedGroups []string, thinkingSuffix string) []map[string]interface{} {
	policies := config.GetGroupPolicies()
	if len(policies) == 0 {
		return models
	}

	// 收集要参考的 group 集合
	wantGroups := make(map[string]bool)
	if len(allowedGroups) == 0 {
		for _, p := range policies {
			wantGroups[strings.ToLower(p.Name)] = true
		}
		// 还要包含没策略覆盖的隐式分组——视为不限
		// 这里通过「若未命中任何策略 → 默认放行」实现
	} else {
		for _, g := range allowedGroups {
			gs := strings.TrimSpace(g)
			if gs == "*" {
				return models
			}
			if gs == "" {
				gs = "default"
			}
			wantGroups[strings.ToLower(gs)] = true
		}
	}

	// 命中的策略集合
	matched := make([]config.GroupPolicy, 0, len(policies))
	hasUnpolicied := false
	if len(allowedGroups) == 0 {
		matched = policies
	} else {
		for g := range wantGroups {
			found := false
			for _, p := range policies {
				if strings.EqualFold(p.Name, g) {
					matched = append(matched, p)
					found = true
					break
				}
			}
			if !found {
				// 该分组无策略 → 放行所有模型
				hasUnpolicied = true
			}
		}
	}
	if hasUnpolicied {
		return models
	}
	if len(matched) == 0 {
		return models
	}

	out := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		idRaw, _ := m["id"].(string)
		base := strings.TrimSuffix(idRaw, thinkingSuffix)
		// 策略并集：任一允许 → 保留
		ok := false
		for _, p := range matched {
			if config.GroupAllowsModel(p.Name, base) {
				ok = true
				break
			}
		}
		if ok {
			out = append(out, m)
		}
	}
	return out
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
			// 自动生成 thinking 变体
			models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
		}
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)

			// 检测 403 封禁状态，自动禁用账号
			errMsg := err.Error()
			if strings.Contains(errMsg, "403") && (strings.Contains(errMsg, "temporarily is suspended") || strings.Contains(errMsg, "temporarily suspended")) {
				logger.Warnf("[ModelsCache] Account %s is suspended, auto-disabling", account.Email)

				updatedAccount := *account
				updatedAccount.Enabled = false
				updatedAccount.BanStatus = "BANNED"
				updatedAccount.BanReason = "Kiro temporarily suspended - security precaution"
				updatedAccount.BanTime = time.Now().Unix()

				if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
					logger.Errorf("[ModelsCache] Failed to update account ban status: %v", updateErr)
				}
			}
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		// 检测 403 封禁状态，自动禁用账号
		errMsg := err.Error()
		if strings.Contains(errMsg, "403") && (strings.Contains(errMsg, "temporarily is suspended") || strings.Contains(errMsg, "temporarily suspended")) {
			logger.Warnf("[fetchAndCacheAccountModels] Account %s is suspended, auto-disabling", account.Email)

			updatedAccount := *account
			updatedAccount.Enabled = false
			updatedAccount.BanStatus = "BANNED"
			updatedAccount.BanReason = "Kiro temporarily suspended - security precaution"
			updatedAccount.BanTime = time.Now().Unix()

			if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
				logger.Errorf("[fetchAndCacheAccountModels] Failed to update account ban status: %v", updateErr)
			}
		}
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// 模型别名映射（保留 thinking 后缀；优先使用绑定到当前 Key 的别名）
	var aliasKeyID string
	if entry := matchedApiKeyEntry(r); entry != nil {
		aliasKeyID = entry.ID
	}
	req.Model = applyModelAliasFor(req.Model, aliasKeyID)

	// 获取账号（按模型过滤 + 按 API Key 允许的 group 过滤）
	actualModel, _ := ParseModelAndThinking(req.Model, config.GetThinkingConfig().Suffix)
	allowedGroups := h.resolveAllowedGroups(r)
	account := h.pool.GetNextForModelAndGroups(actualModel, allowedGroups)
	if account == nil {
		if len(allowedGroups) > 0 {
			h.sendClaudeError(w, 503, "api_error", "No available accounts in allowed groups: "+strings.Join(allowedGroups, ","))
			return
		}
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	// 检查并刷新 token
	if err := h.ensureValidToken(account); err != nil {
		h.sendClaudeError(w, 503, "api_error", "Token refresh failed: "+err.Error())
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)
	cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Stream or non-stream
	if req.Stream {
		h.handleClaudeStream(w, r, account, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheUsage, cacheProfile)
	} else {
		h.handleClaudeNonStream(w, r, account, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheUsage, cacheProfile)
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, r *http.Request, account *config.Account, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheUsage promptCacheUsage, cacheProfile *promptCacheProfile) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	msgID := "msg_" + uuid.New().String()
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var toolUses []KiroToolUse
	var nextContentIndex int
	var rawContentBuilder strings.Builder
	var rawThinkingBuilder strings.Builder
	activeBlockIndex := -1
	activeBlockType := ""
	startInputTokens := estimatedInputTokens

	closeActiveBlock := func() {
		if activeBlockIndex < 0 {
			return
		}
		h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": activeBlockIndex,
		})
		activeBlockIndex = -1
		activeBlockType = ""
	}

	startContentBlock := func(blockType string) {
		if activeBlockType == blockType {
			return
		}
		closeActiveBlock()

		idx := nextContentIndex
		nextContentIndex++

		if blockType == "thinking" {
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type":     "thinking",
					"thinking": "",
				},
			})
		} else {
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type": "text",
					"text": "",
				},
			})
		}

		activeBlockIndex = idx
		activeBlockType = blockType
	}

	// Thinking 标签解析状态
	var textBuffer string
	var inThinkingBlock bool
	var dropTagThinking bool
	var thinkingSource thinkingStreamSource

	// 发送文本的辅助函数
	// thinkingState: 0=普通内容, 1=thinking开始, 2=thinking中间, 3=thinking结束
	sendText := func(text string, thinkingState int) {
		if thinkingState == 0 {
			// 普通内容
			if text == "" {
				return
			}
			startContentBlock("text")
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": text},
			})
			return
		}

		if !thinking {
			return
		}

		switch thinkingFormat {
		case "think":
			var outputText string
			switch thinkingState {
			case 1:
				outputText = "<think>" + text
			case 2:
				outputText = text
			case 3:
				outputText = text + "</think>"
			}
			if outputText == "" {
				return
			}
			startContentBlock("text")
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": outputText},
			})
		case "reasoning_content":
			if text == "" {
				return
			}
			startContentBlock("text")
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": text},
			})
		default:
			if thinkingOpts.OmitDisplay {
				if thinkingState == 1 {
					startContentBlock("thinking")
					return
				}
				if thinkingState == 3 {
					if activeBlockType != "thinking" {
						startContentBlock("thinking")
					}
					closeActiveBlock()
				}
				return
			}
			if thinkingState == 3 && text == "" {
				if activeBlockType == "thinking" {
					closeActiveBlock()
				}
				return
			}
			if text != "" {
				startContentBlock("thinking")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "thinking_delta", "thinking": text},
				})
			}
			if thinkingState == 3 && activeBlockType == "thinking" {
				closeActiveBlock()
			}
		}
	}

	// 处理文本，解析 <thinking> 标签
	var thinkingStarted bool
	var eventThinkingOpen bool

	processClaudeText := func(text string, isThinking bool, forceFlush bool) {
		if isThinking && !thinking {
			return
		}

		// 如果是 reasoningContentEvent，直接输出
		if isThinking {
			if !allowReasoningSource(&thinkingSource) {
				return
			}
			if !thinkingStarted {
				sendText(text, 1)
				thinkingStarted = true
				eventThinkingOpen = true
			} else {
				sendText(text, 2)
			}
			return
		}

		if eventThinkingOpen {
			sendText("", 3)
			eventThinkingOpen = false
			thinkingStarted = false
		}

		textBuffer += text

		for {
			if !inThinkingBlock {
				thinkingStart := strings.Index(textBuffer, "<thinking>")
				if thinkingStart != -1 {
					if thinkingStart > 0 {
						sendText(textBuffer[:thinkingStart], 0)
					}
					textBuffer = textBuffer[thinkingStart+10:]
					inThinkingBlock = true
					dropTagThinking = !allowTagSource(&thinkingSource)
					thinkingStarted = false
				} else if forceFlush || len([]rune(textBuffer)) > 50 {
					// 使用 rune 切片来正确处理 Unicode 字符
					runes := []rune(textBuffer)
					safeLen := len(runes)
					if !forceFlush {
						safeLen = max(0, len(runes)-15)
					}
					if safeLen > 0 {
						sendText(string(runes[:safeLen]), 0)
						textBuffer = string(runes[safeLen:])
					}
					break
				} else {
					break
				}
			} else {
				thinkingEnd := strings.Index(textBuffer, "</thinking>")
				if thinkingEnd != -1 {
					content := textBuffer[:thinkingEnd]
					if !dropTagThinking {
						if !thinkingStarted {
							sendText(content, 1)
							sendText("", 3)
						} else {
							sendText(content, 3)
						}
					}
					textBuffer = textBuffer[thinkingEnd+11:]
					inThinkingBlock = false
					dropTagThinking = false
					thinkingStarted = false
				} else if forceFlush {
					if textBuffer != "" {
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(textBuffer, 1)
								sendText("", 3)
							} else {
								sendText(textBuffer, 3)
							}
						}
						textBuffer = ""
					}
					inThinkingBlock = false
					dropTagThinking = false
					thinkingStarted = false
					break
				} else {
					// 流式输出 thinking 块内的内容
					runes := []rune(textBuffer)
					if len(runes) > 20 {
						safeLen := len(runes) - 15
						if safeLen > 0 {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(string(runes[:safeLen]), 1)
									thinkingStarted = true
								} else {
									sendText(string(runes[:safeLen]), 2)
								}
							}
							textBuffer = string(runes[safeLen:])
						}
					}
					break
				}
			}
		}
	}

	// 发送 message_start
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         buildClaudeUsageMap(startInputTokens, 0, cacheUsage, cacheProfile != nil),
		},
	})

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				rawThinkingBuilder.WriteString(text)
			} else {
				rawContentBuilder.WriteString(text)
			}
			processClaudeText(text, isThinking, false)
		},
		OnToolUse: func(tu KiroToolUse) {
			// 先刷新缓冲区
			processClaudeText("", false, true)
			rawContentBuilder.WriteString(tu.Name)
			if b, err := json.Marshal(tu.Input); err == nil {
				rawContentBuilder.Write(b)
			}

			toolUses = append(toolUses, tu)
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    tu.ToolUseID,
					"name":  tu.Name,
					"input": map[string]interface{}{},
				},
			})

			inputJSON, _ := json.Marshal(tu.Input)
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				},
			})

			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnError: func(err error) {},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	err := CallKiroAPI(account, payload, callback)
	if err != nil {
		h.recordFailure()
		getObserveStore().RecordFailure(account.ID, model)
		getObserveStore().RecordError(account.ID, account.Email, model, 0, err.Error())
		getObserveStore().RecordRequest(account.ID, account.Email, model, 0, 0, 0, false)
		h.pool.RecordError(account.ID, isQuotaError(err))
		h.autoSilentIfSuspended(account.ID, err)
		h.checkOverageError(err, account.ID)
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": err.Error()},
		})
		return
	}

	// 刷新剩余缓冲区
	processClaudeText("", false, true)
	if eventThinkingOpen {
		sendText("", 3)
		eventThinkingOpen = false
	}
	closeActiveBlock()

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
	thinkingOutput := rawThinkingBuilder.String()
	if thinking && thinkingOutput == "" && extractedReasoning != "" {
		thinkingOutput = extractedReasoning
	}
	if !thinking {
		thinkingOutput = ""
	}
	outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

	h.recordSuccess(inputTokens, outputTokens, credits)
	getObserveStore().RecordSuccess(account.ID, model, inputTokens, outputTokens, credits)
	getObserveStore().RecordRequest(account.ID, account.Email, model, inputTokens, outputTokens, credits, true)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordApiKeyUsage(r, inputTokens+outputTokens, credits)
	h.promptCache.Update(account.ID, cacheProfile)

	// 发送 message_delta
	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	}

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": stopReason,
		},
		"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
	})

	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// backgroundCooldownSaver 后台定期保存冷却状态
func (h *Handler) backgroundCooldownSaver() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopStatsSaver:
			// 停机前最后保存一次
			if err := h.pool.SaveCooldowns(); err != nil {
				logger.Warnf("[Pool] Failed to save cooldowns on shutdown: %v", err)
			}
			return
		case <-ticker.C:
			if err := h.pool.SaveCooldowns(); err != nil {
				logger.Warnf("[Pool] Failed to save cooldowns: %v", err)
			}
		}
	}
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// recordApiKeyUsage 把请求统计累加到命中的多 Key 表条目（如有）。
// 旧版根 ApiKey / 未启用鉴权的请求 entry 为 nil，不记账。
func (h *Handler) recordApiKeyUsage(r *http.Request, tokens int, credits float64) {
	entry := matchedApiKeyEntry(r)
	if entry == nil {
		return
	}
	go config.UpdateApiKeyStats(entry.ID, 1, tokens, credits, time.Now().Unix())
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// checkOverageError 检测 402 超额错误，自动关闭对应账号的超额使用
func (h *Handler) checkOverageError(err error, accountID string) {
	if err == nil {
		return
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "402") && strings.Contains(errMsg, "OVERAGE") {
		logger.Warnf("[Overage] Detected overage limit error for account %s, disabling AllowOverage", accountID)
		config.DisableAccountOverage(accountID)
	}
}

func (h *Handler) setAccountSilent(accountID, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "silent"
	}
	if len(reason) > 240 {
		reason = reason[:240]
	}
	accounts := config.GetAccounts()
	for _, a := range accounts {
		if a.ID == accountID {
			if a.Silent && a.SilentReason == reason {
				return
			}
			a.Silent = true
			a.Enabled = false
			a.SilentReason = reason
			a.SilentTime = time.Now().Unix()
			if err := config.UpdateAccount(a.ID, a); err != nil {
				logger.Errorf("[Silent] Failed to update account %s: %v", accountID, err)
				return
			}
			h.pool.Reload()
			logger.Warnf("[Silent] Account %s set silent: %s", a.Email, a.SilentReason)
			return
		}
	}
}

func shouldAutoSilentError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "temporarily is suspended") ||
		strings.Contains(errMsg, "temporarily suspended")
}

func (h *Handler) autoSilentIfSuspended(accountID string, err error) {
	if shouldAutoSilentError(err) {
		h.setAccountSilent(accountID, "auto suspended: "+err.Error())
	}
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, r *http.Request, account *config.Account, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheUsage promptCacheUsage, cacheProfile *promptCacheProfile) {
	var content string
	var thinkingContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinkingContent += text
			} else {
				content += text
			}
		},
		OnToolUse: func(tu KiroToolUse) {
			toolUses = append(toolUses, tu)
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnError: func(err error) {},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	err := CallKiroAPI(account, payload, callback)
	if err != nil {
		h.recordFailure()
		getObserveStore().RecordFailure(account.ID, model)
		getObserveStore().RecordError(account.ID, account.Email, model, 0, err.Error())
		getObserveStore().RecordRequest(account.ID, account.Email, model, 0, 0, 0, false)
		h.pool.RecordError(account.ID, isQuotaError(err))
		h.autoSilentIfSuspended(account.ID, err)
		h.checkOverageError(err, account.ID)
		h.sendClaudeError(w, 500, "api_error", err.Error())
		return
	}

	// 合并 thinking 内容（如果有 reasoningContentEvent 的内容）
	thinkingFormat := thinkingOpts.Format
	finalContent, extractedReasoning := extractThinkingFromContent(content)
	rawThinkingContent := thinkingContent
	if thinking && rawThinkingContent == "" && extractedReasoning != "" {
		rawThinkingContent = extractedReasoning
	}
	if !thinking {
		rawThinkingContent = ""
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

	h.recordSuccess(inputTokens, outputTokens, credits)
	getObserveStore().RecordSuccess(account.ID, model, inputTokens, outputTokens, credits)
	getObserveStore().RecordRequest(account.ID, account.Email, model, inputTokens, outputTokens, credits, true)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordApiKeyUsage(r, inputTokens+outputTokens, credits)
	h.promptCache.Update(account.ID, cacheProfile)

	responseThinkingContent := rawThinkingContent
	includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
	if includeEmptyThinkingBlock {
		responseThinkingContent = ""
	}

	if thinking && responseThinkingContent != "" {
		switch thinkingFormat {
		case "think":
			finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
			responseThinkingContent = ""
		case "reasoning_content":
			finalContent = responseThinkingContent + finalContent // Claude 格式不支持 reasoning_content，直接拼接
			responseThinkingContent = ""
		default:
		}
	}

	resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
	resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
	resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
	resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
	if cacheProfile != nil {
		resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
			Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// 模型别名映射（保留 thinking 后缀；优先使用绑定到当前 Key 的别名）
	var aliasKeyID string
	if entry := matchedApiKeyEntry(r); entry != nil {
		aliasKeyID = entry.ID
	}
	req.Model = applyModelAliasFor(req.Model, aliasKeyID)

	actualModel, _ := ParseModelAndThinking(req.Model, config.GetThinkingConfig().Suffix)
	allowedGroups := h.resolveAllowedGroups(r)
	account := h.pool.GetNextForModelAndGroups(actualModel, allowedGroups)
	if account == nil {
		if len(allowedGroups) > 0 {
			h.sendOpenAIError(w, 503, "server_error", "No available accounts in allowed groups: "+strings.Join(allowedGroups, ","))
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		h.sendOpenAIError(w, 503, "server_error", "Token refresh failed")
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	if req.Stream {
		h.handleOpenAIStream(w, r, account, kiroPayload, req.Model, thinking, estimatedInputTokens)
	} else {
		h.handleOpenAINonStream(w, r, account, kiroPayload, req.Model, thinking, estimatedInputTokens)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, r *http.Request, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	var toolCalls []ToolCall
	var toolCallIndex int
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var rawContentBuilder strings.Builder
	var rawReasoningBuilder strings.Builder

	// Thinking 标签解析状态
	var textBuffer string
	var inThinkingBlock bool
	var dropTagThinking bool
	var thinkingSource thinkingStreamSource

	// 发送 chunk 的辅助函数
	// thinkingState: 0=普通内容, 1=thinking开始, 2=thinking中间, 3=thinking结束
	sendChunk := func(content string, thinkingState int) {
		if content == "" && thinkingState == 2 {
			return
		}

		var chunk map[string]interface{}

		if thinkingState > 0 {
			if !thinking {
				return
			}
			// thinking 内容
			switch thinkingFormat {
			case "thinking":
				// 流式输出标签
				var text string
				switch thinkingState {
				case 1: // 开始
					text = "<thinking>" + content
				case 2: // 中间
					text = content
				case 3: // 结束
					text = content + "</thinking>"
				}
				if text == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": text},
						"finish_reason": nil,
					}},
				}
			case "think":
				var text string
				switch thinkingState {
				case 1:
					text = "<think>" + content
				case 2:
					text = content
				case 3:
					text = content + "</think>"
				}
				if text == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": text},
						"finish_reason": nil,
					}},
				}
			default: // "reasoning_content"
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"reasoning_content": content},
						"finish_reason": nil,
					}},
				}
			}
		} else {
			// 普通内容
			if content == "" {
				return
			}
			chunk = map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]string{"content": content},
					"finish_reason": nil,
				}},
			}
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()
	}

	// 处理文本，解析 <thinking> 标签
	// thinkingStarted 用于跟踪是否已发送开始标签
	var thinkingStarted bool
	var eventThinkingOpen bool

	processText := func(text string, isThinking bool, forceFlush bool) {
		if isThinking && !thinking {
			return
		}

		// 如果是 reasoningContentEvent，直接输出
		if isThinking {
			if !allowReasoningSource(&thinkingSource) {
				return
			}
			if !thinkingStarted {
				sendChunk(text, 1) // 开始
				thinkingStarted = true
				eventThinkingOpen = true
			} else {
				sendChunk(text, 2) // 中间
			}
			return
		}

		if eventThinkingOpen {
			sendChunk("", 3)
			eventThinkingOpen = false
			thinkingStarted = false
		}

		textBuffer += text

		for {
			if !inThinkingBlock {
				// 查找 <thinking> 开始标签
				thinkingStart := strings.Index(textBuffer, "<thinking>")
				if thinkingStart != -1 {
					// 输出 thinking 标签之前的内容
					if thinkingStart > 0 {
						sendChunk(textBuffer[:thinkingStart], 0)
					}
					textBuffer = textBuffer[thinkingStart+10:] // 移除 <thinking>
					inThinkingBlock = true
					dropTagThinking = !allowTagSource(&thinkingSource)
					thinkingStarted = false // 重置，准备发送新的开始标签
				} else if forceFlush || len([]rune(textBuffer)) > 50 {
					// 没有找到标签，安全输出（保留可能的部分标签）
					runes := []rune(textBuffer)
					safeLen := len(runes)
					if !forceFlush {
						safeLen = max(0, len(runes)-15)
					}
					if safeLen > 0 {
						sendChunk(string(runes[:safeLen]), 0)
						textBuffer = string(runes[safeLen:])
					}
					break
				} else {
					break
				}
			} else {
				// 在 thinking 块内，查找 </thinking> 结束标签
				thinkingEnd := strings.Index(textBuffer, "</thinking>")
				if thinkingEnd != -1 {
					// 输出 thinking 内容
					content := textBuffer[:thinkingEnd]
					if !dropTagThinking {
						if !thinkingStarted {
							// 一次性输出完整内容（开始+内容+结束）
							sendChunk(content, 1) // 开始
							sendChunk("", 3)      // 结束（空内容，只发结束标签）
						} else {
							// 已经开始了，发送剩余内容和结束
							sendChunk(content, 3) // 结束
						}
					}
					textBuffer = textBuffer[thinkingEnd+11:] // 移除 </thinking>
					inThinkingBlock = false
					dropTagThinking = false
					thinkingStarted = false
				} else if forceFlush {
					// 强制刷新：输出剩余内容
					if textBuffer != "" {
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(textBuffer, 1) // 开始
								sendChunk("", 3)         // 结束
							} else {
								sendChunk(textBuffer, 3) // 结束
							}
						}
						textBuffer = ""
					}
					inThinkingBlock = false
					dropTagThinking = false
					thinkingStarted = false
					break
				} else {
					// 流式输出 thinking 块内的内容
					runes := []rune(textBuffer)
					if len(runes) > 20 {
						safeLen := len(runes) - 15 // 保留可能的 </thinking> 部分
						if safeLen > 0 {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(string(runes[:safeLen]), 1) // 开始
									thinkingStarted = true
								} else {
									sendChunk(string(runes[:safeLen]), 2) // 中间
								}
							}
							textBuffer = string(runes[safeLen:])
						}
					}
					break
				}
			}
		}
	}

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				rawReasoningBuilder.WriteString(text)
			} else {
				rawContentBuilder.WriteString(text)
			}
			processText(text, isThinking, false)
		},
		OnToolUse: func(tu KiroToolUse) {
			// 先刷新缓冲区
			processText("", false, true)

			args, _ := json.Marshal(tu.Input)
			rawContentBuilder.WriteString(tu.Name)
			rawContentBuilder.Write(args)
			tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
			tc.Function.Name = tu.Name
			tc.Function.Arguments = string(args)
			toolCalls = append(toolCalls, tc)

			chunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{{
							"index": toolCallIndex,
							"id":    tu.ToolUseID,
							"type":  "function",
							"function": map[string]string{
								"name":      tu.Name,
								"arguments": string(args),
							},
						}},
					},
					"finish_reason": nil,
				}},
			}
			toolCallIndex++
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnError: func(err error) {},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	err := CallKiroAPI(account, payload, callback)
	if err != nil {
		h.recordFailure()
		getObserveStore().RecordFailure(account.ID, model)
		getObserveStore().RecordError(account.ID, account.Email, model, 0, err.Error())
		getObserveStore().RecordRequest(account.ID, account.Email, model, 0, 0, 0, false)
		h.pool.RecordError(account.ID, isQuotaError(err))
		h.autoSilentIfSuspended(account.ID, err)
		h.checkOverageError(err, account.ID)
		return
	}

	// 刷新剩余缓冲区
	processText("", false, true)
	if eventThinkingOpen {
		sendChunk("", 3)
		eventThinkingOpen = false
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
	reasoningOutput := rawReasoningBuilder.String()
	if thinking && reasoningOutput == "" && extractedReasoning != "" {
		reasoningOutput = extractedReasoning
	}
	if !thinking {
		reasoningOutput = ""
	}
	outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
	for _, tc := range toolCalls {
		outputTokens += estimateApproxTokens(tc.Function.Name)
		outputTokens += estimateApproxTokens(tc.Function.Arguments)
	}

	h.recordSuccess(inputTokens, outputTokens, credits)
	getObserveStore().RecordSuccess(account.ID, model, inputTokens, outputTokens, credits)
	getObserveStore().RecordRequest(account.ID, account.Email, model, inputTokens, outputTokens, credits, true)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordApiKeyUsage(r, inputTokens+outputTokens, credits)

	// 发送结束
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, r *http.Request, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int) {
	var content string
	var reasoningContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				reasoningContent += text
			} else {
				content += text
			}
		},
		OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
		OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
		OnError: func(err error) {},
		OnCredits: func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	err := CallKiroAPI(account, payload, callback)
	if err != nil {
		h.recordFailure()
		getObserveStore().RecordFailure(account.ID, model)
		getObserveStore().RecordError(account.ID, account.Email, model, 0, err.Error())
		getObserveStore().RecordRequest(account.ID, account.Email, model, 0, 0, 0, false)
		h.pool.RecordError(account.ID, isQuotaError(err))
		h.autoSilentIfSuspended(account.ID, err)
		h.checkOverageError(err, account.ID)
		h.sendOpenAIError(w, 500, "server_error", err.Error())
		return
	}

	// 解析 content 中的 <thinking> 标签
	finalContent, extractedReasoning := extractThinkingFromContent(content)
	if thinking && reasoningContent == "" && extractedReasoning != "" {
		reasoningContent = extractedReasoning
	} else if !thinking {
		reasoningContent = ""
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

	h.recordSuccess(inputTokens, outputTokens, credits)
	getObserveStore().RecordSuccess(account.ID, model, inputTokens, outputTokens, credits)
	getObserveStore().RecordRequest(account.ID, account.Email, model, inputTokens, outputTokens, credits, true)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordApiKeyUsage(r, inputTokens+outputTokens, credits)

	thinkingFormat := config.GetThinkingConfig().OpenAIFormat
	resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// isQuotaError 检测配额耗尽错误
func isQuotaError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "429") ||
		strings.Contains(errMsg, "402") ||
		strings.Contains(errMsg, "quota") ||
		strings.Contains(errMsg, "reached the limit") ||
		strings.Contains(errMsg, "MONTHLY_REQUEST_COUNT") ||
		strings.Contains(errMsg, "DAILY_REQUEST_COUNT")
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		h.autoSilentIfSuspended(account.ID, err)
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
}

// ==================== 管理 API ====================

// handleAdminAPI 处理 admin API 请求
func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// SSE 端点：EventSource 不支持自定义 header，鉴权走 query string
	if r.URL.Path == "/admin/api/events" && r.Method == "GET" {
		h.apiEventsStream(w, r)
		return
	}

	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	if password != config.GetPassword() {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/apikeys" && r.Method == "GET":
		h.apiGetApiKeys(w, r)
	case path == "/apikeys" && r.Method == "POST":
		h.apiUpdateApiKeys(w, r)
	case path == "/groups" && r.Method == "GET":
		h.apiGetGroups(w, r)
	case path == "/group-policies" && r.Method == "GET":
		h.apiGetGroupPolicies(w, r)
	case path == "/group-policies" && r.Method == "POST":
		h.apiUpdateGroupPolicies(w, r)
	case path == "/model-aliases" && r.Method == "GET":
		h.apiGetModelAliases(w, r)
	case path == "/model-aliases" && r.Method == "POST":
		h.apiUpdateModelAliases(w, r)
	case path == "/observe/overview" && r.Method == "GET":
		h.apiObserveOverview(w, r)
	case path == "/observe/account-heatmap" && r.Method == "GET":
		h.apiObserveHeatmap(w, r)
	case path == "/observe/keys" && r.Method == "GET":
		h.apiObserveKeys(w, r)
	case path == "/observe/model-mix" && r.Method == "GET":
		h.apiObserveModelMix(w, r)
	case path == "/observe/recent-errors" && r.Method == "GET":
		h.apiObserveRecentErrors(w, r)
	case path == "/observe/recent-requests" && r.Method == "GET":
		h.apiObserveRecentRequests(w, r)
	case path == "/alerts" && r.Method == "GET":
		h.apiAlertsList(w, r)
	case path == "/alerts" && r.Method == "POST":
		h.apiAlertsCreate(w, r)
	case path == "/alerts/history" && r.Method == "GET":
		h.apiAlertsHistory(w, r)
	case strings.HasPrefix(path, "/alerts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/alerts/"), "/test")
		h.apiAlertsTest(w, r, id)
	case strings.HasPrefix(path, "/alerts/") && r.Method == "GET":
		h.apiAlertsGet(w, r, strings.TrimPrefix(path, "/alerts/"))
	case strings.HasPrefix(path, "/alerts/") && r.Method == "PUT":
		h.apiAlertsUpdate(w, r, strings.TrimPrefix(path, "/alerts/"))
	case strings.HasPrefix(path, "/alerts/") && r.Method == "DELETE":
		h.apiAlertsDelete(w, r, strings.TrimPrefix(path, "/alerts/"))
	case path == "/backups" && r.Method == "GET":
		h.apiBackupsList(w, r)
	case path == "/backups" && r.Method == "POST":
		h.apiBackupsCreate(w, r)
	case path == "/backups/restore" && r.Method == "POST":
		h.apiBackupsRestoreUpload(w, r)
	case path == "/backups/schedule" && r.Method == "GET":
		h.apiBackupsScheduleGet(w, r)
	case path == "/backups/schedule" && r.Method == "POST":
		h.apiBackupsScheduleUpdate(w, r)
	case strings.HasPrefix(path, "/backups/") && strings.HasSuffix(path, "/download") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/backups/"), "/download")
		h.apiBackupsDownload(w, r, id)
	case strings.HasPrefix(path, "/backups/") && strings.HasSuffix(path, "/restore") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/backups/"), "/restore")
		h.apiBackupsRestore(w, r, id)
	case strings.HasPrefix(path, "/backups/") && r.Method == "GET":
		h.apiBackupsGet(w, r, strings.TrimPrefix(path, "/backups/"))
	case strings.HasPrefix(path, "/backups/") && r.Method == "DELETE":
		h.apiBackupsDelete(w, r, strings.TrimPrefix(path, "/backups/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"silent":            a.Silent,
			"silentReason":      a.SilentReason,
			"silentTime":        a.SilentTime,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"allowOverage":      a.AllowOverage,
			"overageWeight":     a.OverageWeight,
			"proxyURL":          a.ProxyURL,
			"groups":            a.Groups,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
		}
	}

	// 排序支持：?sort=requestCount 或 ?sort=-requestCount（降序）
	sortBy := r.URL.Query().Get("sort")
	if sortBy != "" {
		descending := strings.HasPrefix(sortBy, "-")
		field := strings.TrimPrefix(sortBy, "-")

		sort.Slice(result, func(i, j int) bool {
			var less bool
			switch field {
			case "requestCount":
				less = result[i]["requestCount"].(int) < result[j]["requestCount"].(int)
			case "errorCount":
				less = result[i]["errorCount"].(int) < result[j]["errorCount"].(int)
			case "totalTokens":
				less = result[i]["totalTokens"].(int) < result[j]["totalTokens"].(int)
			case "totalCredits":
				less = result[i]["totalCredits"].(float64) < result[j]["totalCredits"].(float64)
			case "lastUsed":
				less = result[i]["lastUsed"].(int64) < result[j]["lastUsed"].(int64)
			case "email":
				less = result[i]["email"].(string) < result[j]["email"].(string)
			case "usageCurrent":
				less = result[i]["usageCurrent"].(float64) < result[j]["usageCurrent"].(float64)
			case "usagePercent":
				less = result[i]["usagePercent"].(float64) < result[j]["usagePercent"].(float64)
			default:
				return false
			}
			if descending {
				return !less
			}
			return less
		})
	}

	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
		if v {
			if existing.BanStatus != "" && existing.BanStatus != "ACTIVE" {
				existing.Enabled = false
			} else {
				existing.Silent = false
				existing.SilentReason = ""
				existing.SilentTime = 0
			}
		}
	}
	if v, ok := updates["silent"].(bool); ok {
		existing.Silent = v
		if v {
			existing.Enabled = false
			if reason, ok := updates["silentReason"].(string); ok && strings.TrimSpace(reason) != "" {
				existing.SilentReason = strings.TrimSpace(reason)
				if len(existing.SilentReason) > 240 {
					existing.SilentReason = existing.SilentReason[:240]
				}
			} else if existing.SilentReason == "" {
				existing.SilentReason = "manual"
			}
			if existing.SilentTime == 0 {
				existing.SilentTime = time.Now().Unix()
			}
		} else {
			if existing.BanStatus == "" || existing.BanStatus == "ACTIVE" {
				existing.Enabled = true
			}
			existing.SilentReason = ""
			existing.SilentTime = 0
		}
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["allowOverage"].(bool); ok {
		existing.AllowOverage = v
	}
	if v, ok := updates["overageWeight"].(float64); ok {
		existing.OverageWeight = clampInt(int(v), 1, 10)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}
	if v, ok := updates["groups"].([]interface{}); ok {
		groups := make([]string, 0, len(v))
		for _, g := range v {
			if s, ok := g.(string); ok && strings.TrimSpace(s) != "" {
				groups = append(groups, strings.TrimSpace(s))
			}
		}
		existing.Groups = groups
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新/分组）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh", "setGroup"
		Group  string   `json:"group"`  // 向后兼容：单个分组
		Groups []string `json:"groups"` // 新格式：多个分组
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "setStandby", "removeStandby":
		standby := req.Action == "setStandby"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		for _, a := range accounts {
			if idSet[a.ID] {
				a.Standby = standby
				if standby {
					a.StandbyTime = time.Now().Unix()
				} else {
					a.StandbyTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "setSilent", "removeSilent":
		silent := req.Action == "setSilent"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		for _, a := range accounts {
			if idSet[a.ID] {
				a.Silent = silent
				if silent {
					// 加入静默：禁用账号
					a.Enabled = false
					a.SilentReason = "manual batch"
					a.SilentTime = time.Now().Unix()
				} else {
					// 移除静默：仅恢复非封禁账号
					if a.BanStatus == "" || a.BanStatus == "ACTIVE" {
						a.Enabled = true
					}
					a.SilentReason = ""
					a.SilentTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				isBanned := a.BanStatus != "" && a.BanStatus != "ACTIVE"
				if enabled {
					if isBanned {
						a.Enabled = false
					} else {
						if !a.Enabled && a.AccessToken != "" {
							toRefreshModels = append(toRefreshModels, a)
						}
						a.Enabled = true
						a.Silent = false
						a.SilentReason = ""
						a.SilentTime = 0
					}
				} else {
					a.Enabled = false
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// 刷新 token
			if account.RefreshToken != "" {
				if newAccess, newRefresh, newExpires, profileArn, err := auth.RefreshToken(account); err == nil {
					account.AccessToken = newAccess
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
					account.ExpiresAt = newExpires
					config.UpdateAccountToken(id, newAccess, newRefresh, newExpires)
					if profileArn != "" {
						account.ProfileArn = profileArn
						config.UpdateAccountProfileArn(id, profileArn)
					}
					h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	case "setGroup":
		// 支持单个分组（向后兼容）或多个分组
		var groups []string
		if req.Group != "" {
			// 向后兼容：单个分组字符串
			groups = []string{strings.TrimSpace(req.Group)}
		} else if len(req.Groups) > 0 {
			// 新格式：多个分组
			for _, g := range req.Groups {
				if trimmed := strings.TrimSpace(g); trimmed != "" {
					groups = append(groups, trimmed)
				}
			}
		}
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		count := 0
		for _, a := range accounts {
			if idSet[a.ID] {
				a.Groups = groups
				if err := config.UpdateAccount(a.ID, a); err == nil {
					count++
				}
			}
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"count":   count,
			"groups":  groups,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		AuthMethod   string `json:"authMethod"`
		Provider     string `json:"provider"`
		Region       string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.AuthMethod == "" {
		if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// 标准化 authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "idc", "builderid", "enterprise":
		req.AuthMethod = "idc"
	case "social", "google", "github":
		req.AuthMethod = "social"
	default:
		if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// 始终尝试用 refreshToken 刷新获取新的 accessToken
	var accessToken string
	var expiresAt int64
	tempAccount := &config.Account{
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Region:       req.Region,
	}
	newAccessToken, newRefreshToken, newExpiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
	if err != nil {
		// 刷新失败，如果有传入的 accessToken 则尝试使用
		if req.AccessToken != "" {
			accessToken = req.AccessToken
			expiresAt = time.Now().Unix() + 300 // 可能已过期，设短一点
		} else {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	} else {
		accessToken = newAccessToken
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
		expiresAt = newExpiresAt
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 检查是否已存在相同 email 的账号（去重）
	existingAccounts := config.GetAccounts()
	var existingAccount *config.Account
	for i := range existingAccounts {
		if existingAccounts[i].Email == email {
			existingAccount = &existingAccounts[i]
			break
		}
	}

	if existingAccount != nil {
		// 账号已存在，跳过导入
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{
				"id":    existingAccount.ID,
				"email": existingAccount.Email,
			},
			"skipped": true,
		})
		return
	}

	// 账号不存在，创建新账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Provider:     req.Provider,
		Region:       req.Region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   newProfileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	// 统计封禁账号与额度耗尽账号
	accounts := config.GetAccounts()
	totalBanned := 0
	todayBanned := 0
	totalExhausted := 0
	todayStart := time.Now().Truncate(24 * time.Hour).Unix()

	for _, a := range accounts {
		if a.BanStatus != "" && a.BanStatus != "ACTIVE" {
			totalBanned++
			if a.BanTime >= todayStart {
				todayBanned++
			}
		}
		if a.UsageLimit > 0 && a.UsageCurrent >= a.UsageLimit {
			totalExhausted++
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,
		"groupBreakdown":  h.pool.GroupStats(),
		"totalBanned":     totalBanned,
		"todayBanned":     todayBanned,
		"totalExhausted":  totalExhausted,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":         config.GetApiKey(),
		"requireApiKey":  config.IsApiKeyRequired(),
		"port":           config.GetPort(),
		"host":           config.GetHost(),
		"allowOverUsage": config.GetAllowOverUsage(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey         string `json:"apiKey"`
		RequireApiKey  bool   `json:"requireApiKey"`
		Password       string `json:"password"`
		AllowOverUsage *bool  `json:"allowOverUsage,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettings(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	getObserveStore().Reset()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误（models unavailable 表示账号被封禁）
		errMsg := err.Error()
		if strings.Contains(errMsg, "models unavailable") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 其他错误返回失败
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"allowOverage":      account.AllowOverage,
		"overageWeight":     account.OverageWeight,
		"proxyURL":          account.ProxyURL,
		"groups":            account.Groups,
		"enabled":           account.Enabled,
		"silent":            account.Silent,
		"silentReason":      account.SilentReason,
		"silentTime":        account.SilentTime,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		// 检测 403 封禁状态，自动禁用账号
		errMsg := err.Error()
		if strings.Contains(errMsg, "403") && (strings.Contains(errMsg, "temporarily is suspended") || strings.Contains(errMsg, "temporarily suspended")) {
			logger.Warnf("[apiRefreshAccountModels] Account %s is suspended, auto-disabling", account.Email)

			updatedAccount := *account
			updatedAccount.Enabled = false
			updatedAccount.BanStatus = "BANNED"
			updatedAccount.BanReason = "Kiro temporarily suspended - security precaution"
			updatedAccount.BanTime = time.Now().Unix()

			if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
				logger.Errorf("[apiRefreshAccountModels] Failed to update account ban status: %v", updateErr)
			}
		}

		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiGetApiKeys 获取多 Key 列表（脱敏）
func (h *Handler) apiGetApiKeys(w http.ResponseWriter, r *http.Request) {
	keys := config.GetApiKeys()
	limiter := getApiKeyLimiter()
	out := make([]map[string]interface{}, 0, len(keys))
	for _, k := range keys {
		usedMin, usedDay := limiter.Snapshot(k.ID)
		out = append(out, map[string]interface{}{
			"id":           k.ID,
			"name":         k.Name,
			"key":          k.Key,
			"groups":       k.Groups,
			"enabled":      k.Enabled,
			"rpm":          k.RPM,
			"rpd":          k.RPD,
			"maxTokens":    k.MaxTokens,
			"requestCount": k.RequestCount,
			"totalTokens":  k.TotalTokens,
			"totalCredits": k.TotalCredits,
			"lastUsed":     k.LastUsed,
			"usedMinute":   usedMin,
			"usedDay":      usedDay,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"keys": out})
}

// apiUpdateApiKeys 全量替换多 Key 列表
func (h *Handler) apiUpdateApiKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []config.ApiKeyEntry `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	// 取旧表用于保留 stats（前端只回写配置字段）
	oldByKey := make(map[string]config.ApiKeyEntry)
	oldByID := make(map[string]config.ApiKeyEntry)
	for _, k := range config.GetApiKeys() {
		if k.Key != "" {
			oldByKey[k.Key] = k
		}
		if k.ID != "" {
			oldByID[k.ID] = k
		}
	}
	// 校验 + 清洗
	cleaned := make([]config.ApiKeyEntry, 0, len(req.Keys))
	seenKeys := make(map[string]bool)
	for _, k := range req.Keys {
		k.Key = strings.TrimSpace(k.Key)
		k.Name = strings.TrimSpace(k.Name)
		if k.Key == "" {
			continue
		}
		if seenKeys[k.Key] {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Duplicate key: " + k.Key})
			return
		}
		seenKeys[k.Key] = true
		if k.ID == "" {
			k.ID = auth.GenerateAccountID()
		}
		// 清理 groups
		groups := make([]string, 0, len(k.Groups))
		for _, g := range k.Groups {
			gs := strings.TrimSpace(g)
			if gs != "" {
				groups = append(groups, gs)
			}
		}
		k.Groups = groups
		// 限流字段下限保护
		if k.RPM < 0 {
			k.RPM = 0
		}
		if k.RPD < 0 {
			k.RPD = 0
		}
		if k.MaxTokens < 0 {
			k.MaxTokens = 0
		}
		// 保留 stats（按 ID 优先，再按 Key 兜底）
		if prev, ok := oldByID[k.ID]; ok {
			k.RequestCount = prev.RequestCount
			k.TotalTokens = prev.TotalTokens
			k.TotalCredits = prev.TotalCredits
			k.LastUsed = prev.LastUsed
		} else if prev, ok := oldByKey[k.Key]; ok {
			k.RequestCount = prev.RequestCount
			k.TotalTokens = prev.TotalTokens
			k.TotalCredits = prev.TotalCredits
			k.LastUsed = prev.LastUsed
		}
		cleaned = append(cleaned, k)
	}
	if err := config.UpdateApiKeys(cleaned); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetGroups 返回分组策略名称列表（用于 UI 分组选择）
func (h *Handler) apiGetGroups(w http.ResponseWriter, r *http.Request) {
	policies := config.GetGroupPolicies()
	groups := make([]string, 0, len(policies))
	seen := make(map[string]bool)
	for _, p := range policies {
		name := strings.TrimSpace(p.Name)
		if name != "" && !seen[name] {
			groups = append(groups, name)
			seen[name] = true
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"groups": groups,
	})
}

// apiGetModelAliases 返回所有模型别名映射
func (h *Handler) apiGetModelAliases(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"aliases": config.GetModelAliases(),
	})
}

// apiUpdateModelAliases 全量替换模型别名映射表
func (h *Handler) apiUpdateModelAliases(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Aliases []config.ModelAlias `json:"aliases"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 已有 ApiKey ID 集合：用于过滤别名 KeyIDs 中的过期 / 未知绑定，
	// 让前端可以静默清掉旧绑定而不必显式更新。
	validKeyIDs := make(map[string]bool)
	for _, k := range config.GetApiKeys() {
		if id := strings.TrimSpace(k.ID); id != "" {
			validKeyIDs[id] = true
		}
	}

	cleaned := make([]config.ModelAlias, 0, len(req.Aliases))
	seen := make(map[string]bool)
	for _, a := range req.Aliases {
		from := strings.TrimSpace(a.From)
		to := strings.TrimSpace(a.To)
		if from == "" || to == "" {
			continue
		}
		key := strings.ToLower(from)
		if seen[key] {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Duplicate alias from: " + from})
			return
		}
		seen[key] = true

		// 清洗 KeyIDs：Trim、去空、去重，并丢弃不存在于 ApiKeys 表中的 ID
		var keyIDs []string
		if len(a.KeyIDs) > 0 {
			seenID := make(map[string]bool, len(a.KeyIDs))
			for _, raw := range a.KeyIDs {
				id := strings.TrimSpace(raw)
				if id == "" || seenID[id] {
					continue
				}
				if !validKeyIDs[id] {
					continue
				}
				seenID[id] = true
				keyIDs = append(keyIDs, id)
			}
		}

		cleaned = append(cleaned, config.ModelAlias{
			From:    from,
			To:      to,
			Enabled: a.Enabled,
			Note:    strings.TrimSpace(a.Note),
			KeyIDs:  keyIDs,
		})
	}
	if err := config.UpdateModelAliases(cleaned); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyModelAlias 在 ParseModelAndThinking 之前对模型名做别名解析。
// 保留旧签名作 wrapper；不带 keyID → 仅命中全局别名。
func applyModelAlias(model string) string {
	return applyModelAliasFor(model, "")
}

// applyModelAliasFor 等价于 applyModelAlias，但允许传入命中多 Key 表的 entry ID，
// 优先返回绑定到该 Key 的别名；若无绑定命中再退回全局别名。
// 保留 thinking 后缀：alias 仅匹配去掉 thinking 后缀的部分。
func applyModelAliasFor(model, keyID string) string {
	suffix := config.GetThinkingConfig().Suffix
	hasThinking := suffix != "" && strings.HasSuffix(strings.ToLower(model), strings.ToLower(suffix))
	base := model
	if hasThinking {
		base = strings.TrimSuffix(model, suffix)
		// 大小写兼容：如果上一步未剪裁（前缀大小写差异），再做一次显式剪裁
		if base == model {
			base = model[:len(model)-len(suffix)]
		}
	}
	resolved := config.ResolveModelAliasFor(base, keyID)
	if hasThinking {
		return resolved + suffix
	}
	return resolved
}
func (h *Handler) apiGetGroupPolicies(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"policies": config.GetGroupPolicies(),
	})
}

// apiUpdateGroupPolicies 全量替换分组模型策略表
func (h *Handler) apiUpdateGroupPolicies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policies []config.GroupPolicy `json:"policies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	cleaned := make([]config.GroupPolicy, 0, len(req.Policies))
	seen := make(map[string]bool)
	for _, p := range req.Policies {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Duplicate group policy: " + name})
			return
		}
		seen[key] = true
		// 清洗 allowed / deny 列表
		allowed := make([]string, 0, len(p.AllowedModels))
		for _, m := range p.AllowedModels {
			ms := strings.TrimSpace(m)
			if ms != "" {
				allowed = append(allowed, ms)
			}
		}
		deny := make([]string, 0, len(p.DenyModels))
		for _, m := range p.DenyModels {
			ms := strings.TrimSpace(m)
			if ms != "" {
				deny = append(deny, ms)
			}
		}
		cleaned = append(cleaned, config.GroupPolicy{
			Name:          name,
			AllowedModels: allowed,
			DenyModels:    deny,
			Description:   strings.TrimSpace(p.Description),
		})
	}
	if err := config.UpdateGroupPolicies(cleaned); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// apiEventsStream Server-Sent Events 端点：后端账号信息更新时主动推送给前端。
// EventSource 不支持自定义 header，鉴权改走 query string ?password=xxx
func (h *Handler) apiEventsStream(w http.ResponseWriter, r *http.Request) {
	password := r.URL.Query().Get("password")
	if password == "" {
		if cookie, _ := r.Cookie("admin_password"); cookie != nil {
			password = cookie.Value
		}
	}
	if password != config.GetPassword() {
		w.WriteHeader(401)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Nginx: 关闭代理缓冲

	id, ch := getBroadcaster().Subscribe()
	defer getBroadcaster().Unsubscribe(id)

	// 立即发一个 hello，确认连接打开
	fmt.Fprintf(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()

	// 心跳，防止中间代理超时
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// SSE 注释行作为心跳
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(evt)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
