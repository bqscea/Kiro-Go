// Package proxy: admin API routing layer.
//
// This file owns the top-level admin-side request dispatch:
//
//   - handleAdminAPI is the entry point reached from ServeHTTP for any
//     /admin/api/* request. It enforces the constant-time password
//     check, then routes to one of six resource dispatchers below.
//   - dispatchBackupsAPI / dispatchObserveAPI / dispatchAlertsAPI /
//     dispatchAccountsAPI / dispatchAuthAPI each own one resource
//     prefix and return false when no (path, method) case matches,
//     letting the caller fall through to a 404 reply.
//   - dispatchConfigAPI is the table-driven catch-all for the flat
//     "single-endpoint" admin paths (settings, stats, thinking, proxy,
//     ...). New flat endpoints just add a row to configRoutes.
//
// The actual apiXxx implementations still live in their per-resource
// files (handler.go, observe_api.go, alerts_api.go, backups_api.go,
// etc.). This file is intentionally routing-only so that adding or
// reordering an endpoint never requires touching unrelated code.
package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"kiro-go/config"
)

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

	if !config.SecureCompareString(password, config.GetPassword()) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	// 默认对所有 admin 写请求加 256KiB body 上限，防止 io.ReadAll OOM。
	// 大上传端点（如备份恢复）在自己的 handler 内再 limitBody 抬高。
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" || r.Method == "PATCH" {
		limitBody(w, r, MaxAdminBodyBytes)
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" || strings.HasPrefix(path, "/accounts/"):
		if !h.dispatchAccountsAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	case strings.HasPrefix(path, "/auth/"):
		if !h.dispatchAuthAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	case strings.HasPrefix(path, "/observe/"):
		if !h.dispatchObserveAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	case path == "/alerts" || strings.HasPrefix(path, "/alerts/"):
		if !h.dispatchAlertsAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	case path == "/backups" || strings.HasPrefix(path, "/backups/"):
		if !h.dispatchBackupsAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	default:
		if !h.dispatchConfigAPI(w, r, path) {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
		}
	}
}

// dispatchBackupsAPI routes /backups* requests. Returns false if the
// (path, method) combo didn't match any backup endpoint.
func (h *Handler) dispatchBackupsAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
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
		return false
	}
	return true
}

// dispatchObserveAPI routes /observe/* requests (all GETs).
func (h *Handler) dispatchObserveAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != "GET" {
		return false
	}
	switch path {
	case "/observe/overview":
		h.apiObserveOverview(w, r)
	case "/observe/account-heatmap":
		h.apiObserveHeatmap(w, r)
	case "/observe/keys":
		h.apiObserveKeys(w, r)
	case "/observe/model-mix":
		h.apiObserveModelMix(w, r)
	case "/observe/recent-errors":
		h.apiObserveRecentErrors(w, r)
	case "/observe/recent-requests":
		h.apiObserveRecentRequests(w, r)
	case "/observe/account-events":
		h.apiObserveAccountEvents(w, r)
	default:
		return false
	}
	return true
}

// dispatchAlertsAPI routes /alerts* requests. Order matters:
// /alerts/history (exact GET) must precede the generic /alerts/{id}
// GET, and /alerts/{id}/test (POST) must precede other suffix paths.
func (h *Handler) dispatchAlertsAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
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
	default:
		return false
	}
	return true
}

// dispatchAccountsAPI routes /accounts* requests. Case order matters:
// /accounts/models/refresh must precede the generic /accounts/{id}/refresh
// (otherwise the literal "models" would be parsed as an account id), and
// the more-specific suffix matches (/models/refresh, /models/cached,
// /models, /full, /test, /refresh) must precede the bare DELETE/PUT/{id}.
func (h *Handler) dispatchAccountsAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
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
	default:
		return false
	}
	return true
}

// dispatchAuthAPI routes /auth/* requests (all POSTs).
func (h *Handler) dispatchAuthAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != "POST" {
		return false
	}
	switch path {
	case "/auth/iam-sso/start":
		h.apiStartIamSso(w, r)
	case "/auth/iam-sso/complete":
		h.apiCompleteIamSso(w, r)
	case "/auth/builderid/start":
		h.apiStartBuilderIdLogin(w, r)
	case "/auth/builderid/poll":
		h.apiPollBuilderIdAuth(w, r)
	case "/auth/sso-token":
		h.apiImportSsoToken(w, r)
	case "/auth/credentials":
		h.apiImportCredentials(w, r)
	default:
		return false
	}
	return true
}

// adminRouteKey keys the flat (path, method) admin endpoint table.
type adminRouteKey struct {
	path   string
	method string
}

// configRoutes is a table-driven dispatch table for the residual admin
// endpoints that don't fit the resource-grouped dispatchers above. Each
// entry is just (path, method) → handler; method expressions let us
// store the bound function once at init time.
var configRoutes = map[adminRouteKey]func(*Handler, http.ResponseWriter, *http.Request){
	{"/status", "GET"}:                (*Handler).apiGetStatus,
	{"/settings", "GET"}:               (*Handler).apiGetSettings,
	{"/settings", "POST"}:              (*Handler).apiUpdateSettings,
	{"/stats", "GET"}:                  (*Handler).apiGetStats,
	{"/stats/reset", "POST"}:           (*Handler).apiResetStats,
	{"/account-events/reset", "POST"}:  (*Handler).apiResetAccountEvents,
	{"/generate-machine-id", "GET"}:    (*Handler).apiGenerateMachineId,
	{"/thinking", "GET"}:               (*Handler).apiGetThinkingConfig,
	{"/thinking", "POST"}:              (*Handler).apiUpdateThinkingConfig,
	{"/endpoint", "GET"}:               (*Handler).apiGetEndpointConfig,
	{"/endpoint", "POST"}:              (*Handler).apiUpdateEndpointConfig,
	{"/proxy", "GET"}:                  (*Handler).apiGetProxy,
	{"/proxy", "POST"}:                 (*Handler).apiUpdateProxy,
	{"/prompt-filter", "GET"}:          (*Handler).apiGetPromptFilter,
	{"/prompt-filter", "POST"}:         (*Handler).apiUpdatePromptFilter,
	{"/version", "GET"}:                (*Handler).apiGetVersion,
	{"/export", "POST"}:                (*Handler).apiExportAccounts,
	{"/apikeys", "GET"}:                (*Handler).apiGetApiKeys,
	{"/apikeys", "POST"}:               (*Handler).apiUpdateApiKeys,
	{"/groups", "GET"}:                 (*Handler).apiGetGroups,
	{"/group-policies", "GET"}:         (*Handler).apiGetGroupPolicies,
	{"/group-policies", "POST"}:        (*Handler).apiUpdateGroupPolicies,
	{"/model-aliases", "GET"}:          (*Handler).apiGetModelAliases,
	{"/model-aliases", "POST"}:         (*Handler).apiUpdateModelAliases,
}

// dispatchConfigAPI looks up the flat config-endpoint table.
func (h *Handler) dispatchConfigAPI(w http.ResponseWriter, r *http.Request, path string) bool {
	fn, ok := configRoutes[adminRouteKey{path, r.Method}]
	if !ok {
		return false
	}
	fn(h, w, r)
	return true
}
