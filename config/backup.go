// Package config: backup engine.
// 提供 config.json 的快照管理（手动 / 自动 / 定时）+ 列表 / 回滚 / 上传恢复。
// 文件落 data/backups/，目录 0700，文件 0600。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	backupDirName   = "backups"
	autoSubDirName  = ".auto"
	manifestName    = "manifest.json"
	maxAutoKeep     = 20
	defaultManualKeep = 100
)

// BackupEntry 一份快照元数据（不含文件内容本身）
type BackupEntry struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	Kind       string `json:"kind"`              // "manual" | "auto" | "scheduled" | "pre-restore"
	Note       string `json:"note,omitempty"`    // 用户备注
	File       string `json:"file"`              // 相对 backupDir 的文件名
	Size       int64  `json:"size"`
	Sha256     string `json:"sha256"`
	AccountCnt int    `json:"accountCnt,omitempty"`
	ApiKeyCnt  int    `json:"apiKeyCnt,omitempty"`
	Version    string `json:"version,omitempty"`
}

// BackupManifest 索引文件
type BackupManifest struct {
	Updated int64         `json:"updated"`
	Entries []BackupEntry `json:"entries"`
}

// BackupSchedule 定时配置（持久化在主 Config 中）
type BackupSchedule struct {
	Enabled  bool   `json:"enabled,omitempty"`
	Cadence  string `json:"cadence,omitempty"`  // "hourly" | "daily" | "weekly"
	Keep     int    `json:"keep,omitempty"`     // scheduled 类保留份数
	LastRun  int64  `json:"lastRun,omitempty"`  // 最近一次 scheduled 快照 unix
}

// BackupConfig 顶层备份偏好（持久化在 Config 中）
type BackupConfig struct {
	AutoEnabled bool           `json:"autoEnabled,omitempty"` // Save() 前置自动快照开关
	AutoKeep    int            `json:"autoKeep,omitempty"`    // .auto/ 保留份数 (0 = maxAutoKeep)
	Schedule    BackupSchedule `json:"schedule,omitempty"`
}

var (
	backupMu sync.Mutex
)

// backupDir 返回 backups 根目录绝对路径
func backupDir() string {
	return filepath.Join(filepath.Dir(cfgPath), backupDirName)
}

func autoDir() string { return filepath.Join(backupDir(), autoSubDirName) }
func manifestPath() string { return filepath.Join(backupDir(), manifestName) }

func ensureBackupDirs() error {
	if err := os.MkdirAll(backupDir(), 0700); err != nil {
		return err
	}
	return os.MkdirAll(autoDir(), 0700)
}

// loadManifest 读取索引（不存在则返回空）
func loadManifest() (*BackupManifest, error) {
	m := &BackupManifest{}
	data, err := os.ReadFile(manifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}

func saveManifest(m *BackupManifest) error {
	m.Updated = time.Now().Unix()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath(), data, 0600)
}

func sha8(s string) string { return s[:8] }

// makeID 时间戳 + sha 前缀
func makeID(now time.Time, sum string) string {
	return fmt.Sprintf("%s-%s", now.UTC().Format("20060102-150405"), sha8(sum))
}

// computeSha256File 计算文件 sha256
func computeSha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// countFromBytes 解析 backup 内容粗略点数（不强求成功）
func countFromBytes(data []byte) (accounts, keys int, version string) {
	var c struct {
		Accounts []struct{} `json:"accounts"`
		ApiKeys  []struct{} `json:"apiKeys"`
		Version  string     `json:"version"`
	}
	_ = json.Unmarshal(data, &c)
	return len(c.Accounts), len(c.ApiKeys), c.Version
}

// CreateBackup 立即拍一份快照。kind: manual / scheduled / pre-restore。
// 内容直接复制当前 cfgPath；如果 kind=="auto" 则落 autoDir。
func CreateBackup(kind, note string) (*BackupEntry, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	return createBackupLocked(kind, note)
}

func createBackupLocked(kind, note string) (*BackupEntry, error) {
	if cfgPath == "" {
		return nil, fmt.Errorf("config path not initialized")
	}
	if err := ensureBackupDirs(); err != nil {
		return nil, err
	}
	srcData, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sum := sha256.Sum256(srcData)
	sumHex := hex.EncodeToString(sum[:])
	id := makeID(now, sumHex)
	fileName := "config-" + id + ".json"
	var fullPath string
	if kind == "auto" {
		fullPath = filepath.Join(autoDir(), fileName)
	} else {
		fullPath = filepath.Join(backupDir(), fileName)
	}
	if err := os.WriteFile(fullPath, srcData, 0600); err != nil {
		return nil, err
	}
	accountCnt, apiKeyCnt, ver := countFromBytes(srcData)
	entry := BackupEntry{
		ID:         id,
		CreatedAt:  now.Unix(),
		Kind:       kind,
		Note:       note,
		File:       relFile(kind, fileName),
		Size:       int64(len(srcData)),
		Sha256:     sumHex,
		AccountCnt: accountCnt,
		ApiKeyCnt:  apiKeyCnt,
		Version:    ver,
	}
	if kind != "auto" {
		m, err := loadManifest()
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, entry)
		sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].CreatedAt > m.Entries[j].CreatedAt })
		if err := saveManifest(m); err != nil {
			return nil, err
		}
	}
	pruneAutoBackups()
	return &entry, nil
}

func relFile(kind, fileName string) string {
	if kind == "auto" {
		return filepath.Join(autoSubDirName, fileName)
	}
	return fileName
}

// pruneAutoBackups 维护 .auto/ 目录在 autoKeep 内
func pruneAutoBackups() {
	keep := maxAutoKeep
	if cfg != nil && cfg.Backup.AutoKeep > 0 {
		keep = cfg.Backup.AutoKeep
	}
	files, err := os.ReadDir(autoDir())
	if err != nil {
		return
	}
	type ent struct {
		name string
		ts   int64
	}
	var entries []ent
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		entries = append(entries, ent{name: f.Name(), ts: info.ModTime().Unix()})
	}
	if len(entries) <= keep {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ts > entries[j].ts })
	for _, e := range entries[keep:] {
		_ = os.Remove(filepath.Join(autoDir(), e.name))
	}
}

// PruneScheduled 按 schedule.Keep 修剪 scheduled 类条目（保留最新）
func pruneKindLocked(kind string, keep int) error {
	if keep <= 0 {
		return nil
	}
	m, err := loadManifest()
	if err != nil {
		return err
	}
	var kept []BackupEntry
	var ofKind []BackupEntry
	for _, e := range m.Entries {
		if e.Kind == kind {
			ofKind = append(ofKind, e)
		} else {
			kept = append(kept, e)
		}
	}
	sort.Slice(ofKind, func(i, j int) bool { return ofKind[i].CreatedAt > ofKind[j].CreatedAt })
	for i, e := range ofKind {
		if i < keep {
			kept = append(kept, e)
		} else {
			_ = os.Remove(filepath.Join(backupDir(), e.File))
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].CreatedAt > kept[j].CreatedAt })
	m.Entries = kept
	return saveManifest(m)
}

// ListBackups 返回所有 manifest 条目（含 auto 通过扫盘补齐）。
// 默认只返回 manifest 内（不含 auto），autoInclude=true 时附加 auto。
func ListBackups(autoInclude bool) ([]BackupEntry, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	if err := ensureBackupDirs(); err != nil {
		return nil, err
	}
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	out := append([]BackupEntry(nil), m.Entries...)
	if autoInclude {
		auto, err := scanAuto()
		if err == nil {
			out = append(out, auto...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// scanAuto 扫 .auto 目录生成临时 entries
func scanAuto() ([]BackupEntry, error) {
	files, err := os.ReadDir(autoDir())
	if err != nil {
		return nil, err
	}
	var out []BackupEntry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		fullPath := filepath.Join(autoDir(), f.Name())
		sum, size, err := computeSha256File(fullPath)
		if err != nil {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		id := info.ModTime().UTC().Format("20060102-150405") + "-" + sha8(sum)
		out = append(out, BackupEntry{
			ID:        id,
			CreatedAt: info.ModTime().Unix(),
			Kind:      "auto",
			File:      filepath.Join(autoSubDirName, f.Name()),
			Size:      size,
			Sha256:    sum,
		})
	}
	return out, nil
}

// FindBackup 按 ID 查找
func FindBackup(id string) (*BackupEntry, error) {
	all, err := ListBackups(true)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("backup not found: %s", id)
}

// ReadBackupBytes 读取快照原文件字节
func ReadBackupBytes(id string) (*BackupEntry, []byte, error) {
	e, err := FindBackup(id)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(filepath.Join(backupDir(), e.File))
	if err != nil {
		return nil, nil, err
	}
	return e, data, nil
}

// DeleteBackup 删除指定快照（含文件 + manifest 条目）
func DeleteBackup(id string) error {
	backupMu.Lock()
	defer backupMu.Unlock()
	m, err := loadManifest()
	if err != nil {
		return err
	}
	idx := -1
	var target BackupEntry
	for i, e := range m.Entries {
		if e.ID == id {
			idx = i
			target = e
			break
		}
	}
	if idx < 0 {
		// 可能是 auto，尝试直接删文件
		auto, _ := scanAuto()
		for _, e := range auto {
			if e.ID == id {
				return os.Remove(filepath.Join(backupDir(), e.File))
			}
		}
		return fmt.Errorf("backup not found: %s", id)
	}
	if err := os.Remove(filepath.Join(backupDir(), target.File)); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.Entries = append(m.Entries[:idx], m.Entries[idx+1:]...)
	return saveManifest(m)
}

// RestoreBackup 回滚到指定快照。先创建 pre-restore 快照保留当前状态，再覆盖 cfgPath，最后 reload。
func RestoreBackup(id string) error {
	backupMu.Lock()
	defer backupMu.Unlock()
	target, data, err := readBackupBytesLocked(id)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("backup file is not valid JSON")
	}
	var probe Config
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("backup schema mismatch: %v", err)
	}
	// pre-restore
	if _, err := createBackupLocked("pre-restore", "auto before restore "+target.ID); err != nil {
		return fmt.Errorf("pre-restore snapshot failed: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return err
	}
	return reloadLocked()
}

func readBackupBytesLocked(id string) (*BackupEntry, []byte, error) {
	// 复用 ListBackups 但避免再加锁：自己拉
	m, err := loadManifest()
	if err != nil {
		return nil, nil, err
	}
	for _, e := range m.Entries {
		if e.ID == id {
			data, err := os.ReadFile(filepath.Join(backupDir(), e.File))
			if err != nil {
				return nil, nil, err
			}
			return &e, data, nil
		}
	}
	// auto
	auto, _ := scanAuto()
	for _, e := range auto {
		if e.ID == id {
			data, err := os.ReadFile(filepath.Join(backupDir(), e.File))
			if err != nil {
				return nil, nil, err
			}
			return &e, data, nil
		}
	}
	return nil, nil, fmt.Errorf("backup not found: %s", id)
}

// reloadLocked 重新解析磁盘上的 cfgPath，刷新 cfg 内存对象
func reloadLocked() error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfgLock.Lock()
	cfg = &c
	cfgLock.Unlock()
	return nil
}

// RestoreFromBytes 接受用户上传的整段 JSON，校验 + pre-restore + 覆盖。
func RestoreFromBytes(data []byte, note string) error {
	backupMu.Lock()
	defer backupMu.Unlock()
	if !json.Valid(data) {
		return fmt.Errorf("uploaded content is not valid JSON")
	}
	var probe Config
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("schema mismatch: %v", err)
	}
	if _, err := createBackupLocked("pre-restore", "upload "+note); err != nil {
		return fmt.Errorf("pre-restore snapshot failed: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		return err
	}
	return reloadLocked()
}

// AutoSnapshotBeforeSave 在 Save() 之前调用：如果 AutoEnabled 则拍一份到 .auto/
func AutoSnapshotBeforeSave() {
	if cfg == nil || !cfg.Backup.AutoEnabled {
		return
	}
	// 失败不打断 Save 主流程
	_, _ = CreateBackup("auto", "")
}

// GetBackupConfig / UpdateBackupConfig / GetBackupSchedule / UpdateBackupSchedule 暴露给 admin
func GetBackupConfig() BackupConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Backup
}

func UpdateBackupConfig(bc BackupConfig) error {
	cfgLock.Lock()
	cfg.Backup = bc
	cfgLock.Unlock()
	return Save()
}

func GetBackupSchedule() BackupSchedule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Backup.Schedule
}

func UpdateBackupSchedule(s BackupSchedule) error {
	cfgLock.Lock()
	cfg.Backup.Schedule = s
	cfgLock.Unlock()
	return Save()
}

// MarkScheduleRan 更新 LastRun（不触发 Save 自动快照风暴）
func MarkScheduleRan(now int64) {
	cfgLock.Lock()
	cfg.Backup.Schedule.LastRun = now
	cfgLock.Unlock()
}

// PruneScheduled 按 schedule.Keep 修剪 scheduled 类条目
func PruneScheduled() error {
	backupMu.Lock()
	defer backupMu.Unlock()
	keep := 50
	if cfg != nil && cfg.Backup.Schedule.Keep > 0 {
		keep = cfg.Backup.Schedule.Keep
	}
	return pruneKindLocked("scheduled", keep)
}
