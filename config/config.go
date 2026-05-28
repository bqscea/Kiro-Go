// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC) or "social" (GitHub/Google)
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub")
	Region       string `json:"region"`                 // AWS region for OIDC endpoints (auth)
	ApiRegion    string `json:"apiRegion,omitempty"`    // AWS region for Kiro API requests (falls back to Region if empty)
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId    string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn   string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Groups is the account grouping labels used by multi-API-key routing.
	// Empty array is treated as the implicit "default" group.
	// Supports multiple groups per account for flexible routing.
	Groups []string `json:"groups,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Overage behavior after the main usage limit is reached.
	AllowOverage  bool `json:"allowOverage,omitempty"`  // Whether to keep using the account after UsageLimit is reached
	OverageWeight int  `json:"overageWeight,omitempty"` // 1-10, lower values reduce overage request frequency

	// Account status
	Enabled       bool   `json:"enabled"`                 // Whether account is active in the pool
	Silent        bool   `json:"silent,omitempty"`        // Silent mode: account is disabled but not banned
	SilentReason  string `json:"silentReason,omitempty"`  // Reason for silent mode
	SilentTime    int64  `json:"silentTime,omitempty"`    // Timestamp when silent mode was set
	Standby       bool   `json:"standby,omitempty"`       // Standby mode: account is refreshed but not used for requests
	StandbyTime   int64  `json:"standbyTime,omitempty"`   // Timestamp when standby mode was set
	BanStatus     string `json:"banStatus,omitempty"`     // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason     string `json:"banReason,omitempty"`     // Reason for ban/suspension
	BanTime       int64  `json:"banTime,omitempty"`       // Timestamp when ban was detected
	ExhaustedTime int64  `json:"exhaustedTime,omitempty"` // Timestamp when usage limit was exhausted

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
	CreatedAt    int64   `json:"createdAt,omitempty"`    // Account creation timestamp (Unix seconds)
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry binds an API key to a list of allowed account groups.
// Empty Groups (or containing "*") means the key may use all groups.
// Empty Group on an account is treated as the implicit "default" group.
type ApiKeyEntry struct {
	ID      string   `json:"id"`               // Unique entry identifier
	Name    string   `json:"name,omitempty"`   // Human-readable label
	Key     string   `json:"key"`              // The actual API key
	Groups  []string `json:"groups,omitempty"` // Allowed account groups; empty / ["*"] = all
	Enabled bool     `json:"enabled"`          // Whether this entry is active

	// Rate limiting (0 = unlimited)
	RPM       int `json:"rpm,omitempty"`       // Max requests per minute
	RPD       int `json:"rpd,omitempty"`       // Max requests per day
	MaxTokens int `json:"maxTokens,omitempty"` // Max tokens per single request (0 = unlimited)

	// Persistent usage statistics (updated by handler, periodically saved)
	RequestCount int     `json:"requestCount,omitempty"`
	TotalTokens  int     `json:"totalTokens,omitempty"`
	TotalCredits float64 `json:"totalCredits,omitempty"`
	LastUsed     int64   `json:"lastUsed,omitempty"`
}

// GroupPolicy binds an account group to a model whitelist / blacklist.
// AllowedModels empty = no restriction (any model may be served by accounts in this group).
// DenyModels takes precedence over AllowedModels: if a model is in DenyModels it is forbidden,
// even if it appears in AllowedModels.
// Match is case-insensitive on the model id (after thinking-suffix stripping in the handler).
type GroupPolicy struct {
	Name          string   `json:"name"`                    // Group label (matches Account.Group; "default" = implicit empty)
	AllowedModels []string `json:"allowedModels,omitempty"` // Whitelist; empty = allow all
	DenyModels    []string `json:"denyModels,omitempty"`    // Blacklist; checked first
	Description   string   `json:"description,omitempty"`   // Free-form note
}

// ModelAlias maps a client-facing model name to an internal Kiro model.
// Match is case-insensitive on the trimmed `From`. Thinking suffix on the
// incoming model id is preserved across the mapping by the handler.
type ModelAlias struct {
	From    string `json:"from"`           // Client-facing model id (e.g. "gpt-4o")
	To      string `json:"to"`             // Internal model id (e.g. "claude-sonnet-4-6")
	Enabled bool   `json:"enabled"`        // Whether this alias is active
	Note    string `json:"note,omitempty"` // Free-form note

	// KeyIDs binds this alias to specific ApiKeyEntry.ID values.
	// Empty = global alias (applies to all callers, including legacy root ApiKey and unauthenticated mode).
	KeyIDs []string `json:"keyIds,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string `json:"password"`         // Admin panel password
	Port          int    `json:"port"`             // HTTP server port (default: 8080)
	Host          string `json:"host"`             // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string `json:"apiKey,omitempty"` // API key for client authentication (legacy single key)
	RequireApiKey bool   `json:"requireApiKey"`    // Whether to enforce API key validation
	// ApiKeys is the multi-key table. Each entry can be scoped to one or more
	// account groups. A request authenticated with an entry will only be routed
	// to accounts whose Group matches the entry's Groups (empty / "*" = any).
	ApiKeys []ApiKeyEntry `json:"apiKeys,omitempty"`

	// GroupPolicies binds account groups to model whitelist/blacklist.
	// Routing first filters by API key allowed groups, then by group's model policy.
	GroupPolicies []GroupPolicy `json:"groupPolicies,omitempty"`

	// ModelAliases maps client-facing model names to internal Kiro models.
	// Applied before model routing; thinking suffix is preserved.
	ModelAliases  []ModelAlias `json:"modelAliases,omitempty"`
	KiroVersion   string       `json:"kiroVersion,omitempty"`
	SystemVersion string       `json:"systemVersion,omitempty"`
	NodeVersion   string       `json:"nodeVersion,omitempty"`
	Accounts      []Account    `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// LoadBalancingMode controls account selection strategy.
	// "priority" (按优先级): tries accounts in priority order (lower Weight = higher priority), falls back on failure
	// "balanced" (均衡分配): weighted round-robin distribution (default)
	LoadBalancingMode string `json:"loadBalancingMode,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Retry configuration for request-level fault tolerance
	MaxRetriesPerAccount int `json:"maxRetriesPerAccount,omitempty"` // Max retries per single account (default: 3)
	MaxRetriesPerRequest int `json:"maxRetriesPerRequest,omitempty"` // Max retries across all accounts (default: 9)
	RetryBaseDelayMs     int `json:"retryBaseDelayMs,omitempty"`     // Base delay in milliseconds (default: 100)
	RetryMaxDelayMs      int `json:"retryMaxDelayMs,omitempty"`      // Max delay in milliseconds (default: 5000)

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed

	// Today's statistics (reset daily at midnight)
	TodayRequests int     `json:"todayRequests,omitempty"` // Today's request count
	TodayTokens   int     `json:"todayTokens,omitempty"`   // Today's token count
	TodayCredits  float64 `json:"todayCredits,omitempty"`  // Today's credits consumed
	LastResetDate string  `json:"lastResetDate,omitempty"` // Last reset date (YYYY-MM-DD)

	// Backup configuration
	Backup BackupConfig `json:"backup,omitempty"`

	// Alert configuration
	Alert AlertConfig `json:"alert,omitempty"`
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// TokenRefreshSkewSeconds is the number of seconds before token expiration
// to proactively refresh the token. Unified constant used across packages.
const TokenRefreshSkewSeconds int64 = 300 // 5 minutes

// Version current version
const Version = "1.0.9"

// DefaultPassword is the password applied to a freshly-created config.
// Operators MUST change this before exposing the admin panel to the
// internet — IsDefaultPassword() reports whether the live config still
// uses this value so callers can surface a startup warning.
const DefaultPassword = "changeme"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

func defaultConfig() *Config {
	return &Config{
		Password:      DefaultPassword,
		Port:          8080,
		Host:          "0.0.0.0",
		RequireApiKey: false,
		Accounts:      []Account{},
	}
}

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgPath = path
	if err := Load(); err != nil {
		return err
	}

	// Initialize credentials system
	configDir := filepath.Dir(cfgPath)
	InitCredentials(configDir)

	// Load credentials from credentials.json (optional, not an error if missing)
	if err := LoadCredentials(); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}

	return nil
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = defaultConfig()
			return Save()
		}
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		cfg = defaultConfig()
		return Save()
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c
	return nil
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
func Save() error {
	AutoSnapshotBeforeSave()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(cfgPath, data, 0600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false

	dirFile, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

// IsDefaultPassword reports whether the active admin password is still
// the bundled default (see DefaultPassword). Use this to emit a startup
// warning when an operator has not yet rotated credentials.
func IsDefaultPassword() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password == DefaultPassword
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	// Priority: credentials.json > config.json (backward compatibility)
	if CredentialsLoaded() {
		return GetCredentials()
	}
	// Fallback to config.json
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	// Priority: credentials.json > config.json (backward compatibility)
	all := GetAccounts()
	var accounts []Account
	for _, a := range all {
		if a.Enabled && !a.Silent && !a.Standby && (a.BanStatus == "" || a.BanStatus == "ACTIVE") {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		return AddCredential(account)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

func UpdateAccount(id string, account Account) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		return UpdateCredential(account)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// DisableAccountOverage turns off AllowOverage for a specific account.
func DisableAccountOverage(id string) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		acc := GetCredentialByID(id)
		if acc == nil {
			return nil
		}
		acc.AllowOverage = false
		return UpdateCredential(*acc)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AllowOverage = false
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		return UpdateCredentialProfileArn(id, profileArn)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		return RemoveCredential(id)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	// Priority: credentials.json > config.json (writeback)
	if CredentialsLoaded() {
		return UpdateCredentialToken(id, accessToken, refreshToken, expiresAt)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

// GetApiKeys returns a copy of all configured multi-key entries.
func GetApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// UpdateApiKeys overwrites the multi-key table.
func UpdateApiKeys(keys []ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKeys = keys
	return Save()
}

// UpdateApiKeyStats accumulates usage stats for a multi-key entry by ID.
// Called from handler after each successful proxied request.
func UpdateApiKeyStats(id string, requests, tokens int, credits float64, lastUsed int64) error {
	if id == "" {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].RequestCount += requests
			cfg.ApiKeys[i].TotalTokens += tokens
			cfg.ApiKeys[i].TotalCredits += credits
			if lastUsed > cfg.ApiKeys[i].LastUsed {
				cfg.ApiKeys[i].LastUsed = lastUsed
			}
			return Save()
		}
	}
	return nil
}

// FindApiKeyEntry returns the matching enabled entry from the multi-key table.
// Returns nil if no entry matches or the key is disabled.
func FindApiKeyEntry(key string) *ApiKeyEntry {
	if key == "" {
		return nil
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		e := &cfg.ApiKeys[i]
		if e.Enabled && SecureCompareString(e.Key, key) {
			out := *e
			return &out
		}
	}
	return nil
}

// GetAccountGroups returns all unique non-empty group labels currently used by accounts.
func GetAccountGroups() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, a := range cfg.Accounts {
		for _, g := range a.Groups {
			if g == "" || seen[g] {
				continue
			}
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// GetGroupPolicies returns a copy of all configured group policies.
func GetGroupPolicies() []GroupPolicy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]GroupPolicy, len(cfg.GroupPolicies))
	copy(out, cfg.GroupPolicies)
	return out
}

// UpdateGroupPolicies overwrites the group policy table.
func UpdateGroupPolicies(policies []GroupPolicy) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.GroupPolicies = policies
	return Save()
}

// GetGroupPolicy returns the policy bound to the named group (case-insensitive),
// or nil if none. Empty / "default" name normalisation is up to the caller.
func GetGroupPolicy(name string) *GroupPolicy {
	if name == "" {
		name = "default"
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.GroupPolicies {
		if strings.EqualFold(cfg.GroupPolicies[i].Name, name) {
			out := cfg.GroupPolicies[i]
			return &out
		}
	}
	return nil
}

// GroupAllowsModel reports whether the named group's policy permits this model.
// Empty group / "default" maps to the policy named "default" if any.
// No policy = unrestricted. DenyModels checked first; AllowedModels empty = allow all.
// Match is case-insensitive on the trimmed model id.
func GroupAllowsModel(groupName, model string) bool {
	policy := GetGroupPolicy(groupName)
	if policy == nil {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, dm := range policy.DenyModels {
		if strings.EqualFold(strings.TrimSpace(dm), m) {
			return false
		}
	}
	if len(policy.AllowedModels) == 0 {
		return true
	}
	for _, am := range policy.AllowedModels {
		if strings.EqualFold(strings.TrimSpace(am), m) {
			return true
		}
	}
	return false
}

// GetModelAliases returns a copy of all configured model aliases.
// KeyIDs slices are deep-copied so callers cannot mutate the in-memory binding list.
func GetModelAliases() []ModelAlias {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ModelAlias, len(cfg.ModelAliases))
	for i, a := range cfg.ModelAliases {
		out[i] = a
		if len(a.KeyIDs) > 0 {
			out[i].KeyIDs = append([]string(nil), a.KeyIDs...)
		} else {
			out[i].KeyIDs = nil
		}
	}
	return out
}

// UpdateModelAliases overwrites the model alias table.
func UpdateModelAliases(aliases []ModelAlias) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ModelAliases = aliases
	return Save()
}

// ResolveModelAlias is a backward-compatible wrapper that resolves an alias
// without considering any caller-bound API key.
func ResolveModelAlias(model string) string {
	return ResolveModelAliasFor(model, "")
}

// ResolveModelAliasFor maps a client-facing model name to its internal target,
// optionally honoring per-key alias bindings.
//
// Resolution order for enabled aliases whose `From` matches (case-insensitive, trimmed):
//  1. The first alias whose KeyIDs contains keyID (only when keyID != "").
//  2. Otherwise, the first global alias (KeyIDs empty).
//  3. Otherwise, the input model is returned unchanged.
//
// The full list is scanned once for a key-bound match before falling back to a
// global match, so iteration order does not cause an early global hit to mask a
// later key-bound one.
func ResolveModelAliasFor(model, keyID string) string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return model
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return model
	}

	var globalHit *ModelAlias
	for i := range cfg.ModelAliases {
		a := &cfg.ModelAliases[i]
		if !a.Enabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.From), m) {
			continue
		}
		to := strings.TrimSpace(a.To)
		if to == "" {
			continue
		}
		if keyID != "" && len(a.KeyIDs) > 0 {
			for _, id := range a.KeyIDs {
				if strings.TrimSpace(id) == keyID {
					return to
				}
			}
			continue
		}
		if len(a.KeyIDs) == 0 && globalHit == nil {
			globalHit = a
		}
	}
	if globalHit != nil {
		return strings.TrimSpace(globalHit.To)
	}
	return model
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// GetTodayStats 获取今日统计
func GetTodayStats() (int, int, float64, string) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TodayRequests, cfg.TodayTokens, cfg.TodayCredits, cfg.LastResetDate
}

// UpdateTodayStats 更新今日统计
func UpdateTodayStats(todayReq, todayTokens int, todayCredits float64, lastResetDate string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TodayRequests = todayReq
	cfg.TodayTokens = todayTokens
	cfg.TodayCredits = todayCredits
	cfg.LastResetDate = lastResetDate
	return Save()
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		return UpdateCredentialStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	// Priority: credentials.json > config.json
	if CredentialsLoaded() {
		// Update Email/UserId if provided
		acc := GetCredentialByID(id)
		if acc != nil {
			if info.Email != "" {
				acc.Email = info.Email
			}
			if info.UserId != "" {
				acc.UserId = info.UserId
			}
			if info.Email != "" || info.UserId != "" {
				if err := UpdateCredential(*acc); err != nil {
					return err
				}
			}
		}
		return UpdateCredentialInfo(id, info)
	}
	// Fallback to config.json
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetLoadBalancingMode returns the load balancing mode. Defaults to "balanced".
func GetLoadBalancingMode() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LoadBalancingMode == "" {
		return "balanced"
	}
	return cfg.LoadBalancingMode
}

// UpdateLoadBalancingMode sets the load balancing mode and persists the change.
func UpdateLoadBalancingMode(mode string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LoadBalancingMode = mode
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return "info"
	}
	if cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// GetRetryConfig returns retry configuration with defaults applied.
func GetRetryConfig() (maxPerAccount, maxPerRequest, baseDelayMs, maxDelayMs int) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	maxPerAccount = 3
	maxPerRequest = 9
	baseDelayMs = 100
	maxDelayMs = 5000

	if cfg != nil {
		if cfg.MaxRetriesPerAccount > 0 {
			maxPerAccount = cfg.MaxRetriesPerAccount
		}
		if cfg.MaxRetriesPerRequest > 0 {
			maxPerRequest = cfg.MaxRetriesPerRequest
		}
		if cfg.RetryBaseDelayMs > 0 {
			baseDelayMs = cfg.RetryBaseDelayMs
		}
		if cfg.RetryMaxDelayMs > 0 {
			maxDelayMs = cfg.RetryMaxDelayMs
		}
	}

	return
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
