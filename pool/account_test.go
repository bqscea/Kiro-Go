package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverageAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		clientBindings:  make(map[string]string),
		bindingLastSeen: make(map[string]time.Time),
	}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext("")
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped by default")
		}
	}
}

func TestOverageAccountsCanBeSelectedWhenAllowed(t *testing.T) {
	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		clientBindings:  make(map[string]string),
		bindingLastSeen: make(map[string]time.Time),
	}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		AllowOverage:  true,
		OverageWeight: 1,
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext("")
	if acc == nil {
		t.Fatalf("expected allowed overage account")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverageWeightIsLowerThanNormalWeight(t *testing.T) {
	normalWeight := effectiveWeight(1) * overageFrequencyScale
	overageWeight := effectiveOverageWeight(1)

	if overageWeight >= normalWeight {
		t.Fatalf("expected overage weight %d to be lower than normal weight %d", overageWeight, normalWeight)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		clientBindings:  make(map[string]string),
		bindingLastSeen: make(map[string]time.Time),
	}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext("")
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetNextForModelAndGroupsExcludingSkipsFailedAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	p := &AccountPool{
		cooldowns:       make(map[string]time.Time),
		modelLists:      make(map[string]map[string]bool),
		clientBindings:  make(map[string]string),
		bindingLastSeen: make(map[string]time.Time),
	}
	p.accounts = []config.Account{
		{ID: "failed", Enabled: true},
		{ID: "healthy", Enabled: true},
	}
	p.modelLists["failed"] = map[string]bool{"claude-sonnet-4.5": true}
	p.modelLists["healthy"] = map[string]bool{"claude-sonnet-4.5": true}

	got := p.GetNextForModelAndGroupsExcluding("claude-sonnet-4.5", nil, map[string]bool{"failed": true}, "")
	if got == nil {
		t.Fatalf("expected healthy account")
	}
	if got.ID != "healthy" {
		t.Fatalf("expected healthy account, got %q", got.ID)
	}
}
