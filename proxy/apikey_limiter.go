package proxy

import (
	"sync"
	"time"
)

// apiKeyLimiter 基于 Key ID 维护分钟桶 / 当日桶 + 累计统计。
// 内存实现，进程重启后即时窗口归零，但累计统计由 config 持久化。
type apiKeyLimiter struct {
	mu      sync.Mutex
	buckets map[string]*keyBucket
}

type keyBucket struct {
	minuteWindow int64 // unix minute
	minuteCount  int
	dayWindow    int64 // unix day
	dayCount     int
}

var (
	keyLimiterOnce sync.Once
	keyLimiter     *apiKeyLimiter
)

func getApiKeyLimiter() *apiKeyLimiter {
	keyLimiterOnce.Do(func() {
		keyLimiter = &apiKeyLimiter{
			buckets: make(map[string]*keyBucket),
		}
	})
	return keyLimiter
}

// Allow 在限流窗口内检查并记账（命中即占用一次配额）。
// rpm/rpd <=0 表示对应维度不限制。返回 (允许, 拒绝原因)。
func (l *apiKeyLimiter) Allow(keyID string, rpm, rpd int) (bool, string) {
	if keyID == "" {
		return true, ""
	}
	if rpm <= 0 && rpd <= 0 {
		return true, ""
	}

	now := time.Now()
	curMin := now.Unix() / 60
	curDay := now.Unix() / 86400

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[keyID]
	if !ok {
		b = &keyBucket{minuteWindow: curMin, dayWindow: curDay}
		l.buckets[keyID] = b
	}
	if b.minuteWindow != curMin {
		b.minuteWindow = curMin
		b.minuteCount = 0
	}
	if b.dayWindow != curDay {
		b.dayWindow = curDay
		b.dayCount = 0
	}

	if rpm > 0 && b.minuteCount >= rpm {
		return false, "rate limit exceeded: RPM"
	}
	if rpd > 0 && b.dayCount >= rpd {
		return false, "rate limit exceeded: RPD"
	}
	b.minuteCount++
	b.dayCount++
	return true, ""
}

// Snapshot 返回某 Key 当前分钟/当日已用次数，用于监控显示。
func (l *apiKeyLimiter) Snapshot(keyID string) (int, int) {
	if keyID == "" {
		return 0, 0
	}
	now := time.Now()
	curMin := now.Unix() / 60
	curDay := now.Unix() / 86400

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[keyID]
	if !ok {
		return 0, 0
	}
	min := b.minuteCount
	day := b.dayCount
	if b.minuteWindow != curMin {
		min = 0
	}
	if b.dayWindow != curDay {
		day = 0
	}
	return min, day
}
