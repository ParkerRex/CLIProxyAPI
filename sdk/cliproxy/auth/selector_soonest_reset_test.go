package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func soonestResetPick(t *testing.T, selector *SoonestResetSelector, provider string, auths []*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func codexWeeklyQuota(observedAt, resetAt time.Time) QuotaState {
	return QuotaState{
		ObservedAt: observedAt,
		Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         strconv.FormatInt(observedAt.Add(time.Hour).Unix(), 10),
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       strconv.FormatInt(resetAt.Unix(), 10),
		},
	}
}

func TestSoonestResetSelectorPrefersEarliestWeeklyReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		{ID: "a", Quota: codexWeeklyQuota(now, now.Add(6*24*time.Hour))},
		{ID: "b", Quota: codexWeeklyQuota(now, now.Add(24*time.Hour))},
		{ID: "c", Quota: codexWeeklyQuota(now, now.Add(3*24*time.Hour))},
	}
	for i := 0; i < 3; i++ {
		if got := soonestResetPick(t, selector, "codex", auths); got != "b" {
			t.Fatalf("pick %d = %q, want b", i, got)
		}
	}
}

func TestSoonestResetSelectorUsesWeeklyWindowNotFiveHour(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// a: five-hour window resets in 10 minutes, weekly in 5 days.
	// b: five-hour window resets in 4 hours, weekly in 2 days.
	// Window order is swapped on b to prove the longer window is chosen by duration.
	a := &Auth{ID: "a", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Reset-At":         strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10),
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(5*24*time.Hour).Unix(), 10),
	}}}
	b := &Auth{ID: "b", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Window-Minutes":   "10080",
		"X-Codex-Primary-Reset-At":         strconv.FormatInt(now.Add(2*24*time.Hour).Unix(), 10),
		"X-Codex-Secondary-Window-Minutes": "300",
		"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10),
	}}}
	if got := soonestResetPick(t, &SoonestResetSelector{}, "codex", []*Auth{a, b}); got != "b" {
		t.Fatalf("Pick() = %q, want b", got)
	}
}

func TestSoonestResetSelectorResolvesRelativeResetFromObservationTime(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// a was observed two days ago with four days left, so it resets in two days.
	// b was observed now with three days left.
	a := &Auth{ID: "a", Quota: QuotaState{ObservedAt: now.Add(-48 * time.Hour), Signals: map[string]string{
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(4 * 24 * 3600),
	}}}
	b := &Auth{ID: "b", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Window-Minutes":      "10080",
		"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(3 * 24 * 3600),
	}}}
	if got := soonestResetPick(t, &SoonestResetSelector{}, "codex", []*Auth{b, a}); got != "a" {
		t.Fatalf("Pick() = %q, want a", got)
	}
}

func TestSoonestResetSelectorReadsClaudeWeeklyReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	claudeQuota := func(fiveHour, weekly time.Duration) QuotaState {
		return QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Reset": strconv.FormatInt(now.Add(fiveHour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(now.Add(weekly).Unix(), 10),
		}}
	}
	auths := []*Auth{
		{ID: "a", Quota: claudeQuota(time.Minute, 5*24*time.Hour)},
		{ID: "b", Quota: claudeQuota(4*time.Hour, 24*time.Hour)},
	}
	if got := soonestResetPick(t, &SoonestResetSelector{}, "claude", auths); got != "b" {
		t.Fatalf("Pick() = %q, want b", got)
	}
}

func TestSoonestResetSelectorIgnoresPastReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		{ID: "a", Quota: codexWeeklyQuota(now.Add(-8*24*time.Hour), now.Add(-time.Hour))},
		{ID: "b", Quota: codexWeeklyQuota(now, now.Add(3*24*time.Hour))},
	}
	// The first pick tries "a" once because its reset is unknown again.
	soonestResetPick(t, selector, "codex", auths)
	for i := 0; i < 3; i++ {
		if got := soonestResetPick(t, selector, "codex", auths); got != "b" {
			t.Fatalf("pick %d = %q, want b", i, got)
		}
	}
}

func TestSoonestResetSelectorTriesUnobservedAccountsOnce(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		{ID: "a", Quota: codexWeeklyQuota(now, now.Add(6*24*time.Hour))},
		{ID: "b"},
		{ID: "c"},
	}
	// Unobserved accounts are tried once each so their reset time can be learned.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		seen[soonestResetPick(t, selector, "codex", auths)] = true
	}
	if !seen["b"] || !seen["c"] {
		t.Fatalf("first picks = %v, want b and c", seen)
	}
	// An account that still reports nothing ranks after accounts with a known reset.
	if got := soonestResetPick(t, selector, "codex", auths); got != "a" {
		t.Fatalf("Pick() = %q, want a", got)
	}
}

func TestSoonestResetSelectorRotatesWhenNoResetIsKnown(t *testing.T) {
	t.Parallel()

	selector := &SoonestResetSelector{}
	auths := []*Auth{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		counts[soonestResetPick(t, selector, "gemini", auths)]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != 3 {
			t.Fatalf("counts = %v, want 3 picks each", counts)
		}
	}
}
