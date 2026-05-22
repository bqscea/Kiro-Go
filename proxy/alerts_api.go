// Package proxy: admin API endpoints for alerts tab.
package proxy

import (
	"encoding/json"
	"net/http"

	"kiro-go/config"
)

// apiAlertsList GET /admin/api/alerts
func (h *Handler) apiAlertsList(w http.ResponseWriter, r *http.Request) {
	rules := config.ListAlertRules()
	json.NewEncoder(w).Encode(map[string]interface{}{"rules": rules})
}

// apiAlertsCreate POST /admin/api/alerts {name, enabled, condition, actions, cooldown}
func (h *Handler) apiAlertsCreate(w http.ResponseWriter, r *http.Request) {
	var rule config.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	created, err := config.CreateAlertRule(rule)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"rule": created})
}

// apiAlertsGet GET /admin/api/alerts/{id}
func (h *Handler) apiAlertsGet(w http.ResponseWriter, r *http.Request, id string) {
	rule, err := config.FindAlertRule(id)
	if err != nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"rule": rule})
}

// apiAlertsUpdate PUT /admin/api/alerts/{id}
func (h *Handler) apiAlertsUpdate(w http.ResponseWriter, r *http.Request, id string) {
	var rule config.AlertRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.UpdateAlertRule(id, rule); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiAlertsDelete DELETE /admin/api/alerts/{id}
func (h *Handler) apiAlertsDelete(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAlertRule(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiAlertsHistory GET /admin/api/alerts/history?limit=50
func (h *Handler) apiAlertsHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := json.Number(l).Int64(); err == nil && parsed > 0 {
			limit = int(parsed)
		}
	}
	history := config.ListAlertHistory(limit)
	json.NewEncoder(w).Encode(map[string]interface{}{"history": history})
}

// apiAlertsTest POST /admin/api/alerts/{id}/test (dry-run evaluation)
func (h *Handler) apiAlertsTest(w http.ResponseWriter, r *http.Request, id string) {
	rule, err := config.FindAlertRule(id)
	if err != nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	triggered, value := h.evaluateRule(rule)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"triggered": triggered,
		"value":     value,
		"threshold": rule.Condition.Threshold,
	})
}
