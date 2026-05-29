// Package proxy: request body size guards.
//
// Every handler that reads r.Body MUST wrap it with http.MaxBytesReader
// before touching io.ReadAll / json.NewDecoder, otherwise a malicious or
// runaway client can OOM the server. This file centralizes the per-tier
// limits and the wrapper helper.
//
// The numbers are calibrated, not arbitrary:
//
//   - MaxChatBodyBytes (32 MiB) — Claude Code and tool-heavy clients can
//     submit large client payloads before proxy-side filtering/truncation.
//     Kiro's smaller upstream payload limit is handled after translation by
//     trimming conversation history, not by rejecting the client request.
//   - MaxAdminBodyBytes (256 KiB) — admin API JSON payloads (settings,
//     account edits, alert rules). Even with hundreds of accounts /
//     model aliases this stays comfortably under the cap.
//   - MaxBackupUploadBytes (64 MiB) — full-config backup restore upload.
//     Backups can be large when accounts + observe DB snapshots are
//     bundled, so this tier is intentionally generous.
package proxy

import "net/http"

const (
	MaxChatBodyBytes     = 32 << 20  // 32 MiB
	MaxAdminBodyBytes    = 256 << 10 // 256 KiB
	MaxBackupUploadBytes = 64 << 20  // 64 MiB
)

// limitBody wraps r.Body with http.MaxBytesReader so subsequent reads
// fail fast (with a 413-like error) once the cap is exceeded. The
// returned error from downstream decoders bubbles up unchanged; existing
// 400 "Invalid JSON" / "Failed to read body" paths already cover it.
func limitBody(w http.ResponseWriter, r *http.Request, n int64) {
	r.Body = http.MaxBytesReader(w, r.Body, n)
}
