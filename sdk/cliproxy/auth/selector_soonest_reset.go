package auth

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector spends the credential whose longest quota window resets first,
// so allowance that is about to refresh is used before it is lost.
//
// Reset times come from the passive quota snapshot recorded on every upstream response
// (Codex, Claude, Devin). A credential without a known future reset is tried once so its
// reset can be learned; after that it ranks behind credentials with a known reset, and
// such credentials rotate round-robin among themselves.
type SoonestResetSelector struct {
	mu       sync.Mutex
	tried    map[string]struct{}
	rotation RoundRobinSelector
}

// Pick selects the available credential with the earliest known quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var soonest *Auth
	var soonestAt time.Time
	unknown := make([]*Auth, 0, len(available))
	s.mu.Lock()
	if s.tried == nil {
		s.tried = make(map[string]struct{})
	}
	for _, candidate := range available {
		resetAt, ok := quotaWindowResetAt(candidate.Quota, now)
		if !ok {
			if _, done := s.tried[candidate.ID]; !done {
				s.tried[candidate.ID] = struct{}{}
				s.mu.Unlock()
				return candidate, nil
			}
			unknown = append(unknown, candidate)
			continue
		}
		if soonest == nil || resetAt.Before(soonestAt) {
			soonest, soonestAt = candidate, resetAt
		}
	}
	s.mu.Unlock()
	if soonest != nil {
		return soonest, nil
	}
	return s.rotation.Pick(ctx, provider, model, opts, unknown)
}

// quotaWindowResetAt returns the future reset time of the longest quota window in the
// snapshot. The longest window (weekly) is the scarce one; short windows refresh anyway.
func quotaWindowResetAt(quota QuotaState, now time.Time) (time.Time, bool) {
	signals := quota.Signals
	if len(signals) == 0 {
		return time.Time{}, false
	}

	var resetAt time.Time
	// Codex reports two windows and does not fix which one is weekly, so rank by duration.
	longestMinutes := int64(-1)
	for _, window := range []string{"Primary", "Secondary"} {
		at, ok := codexWindowResetAt(quota, "X-Codex-"+window+"-")
		if !ok {
			continue
		}
		minutes, _ := strconv.ParseInt(strings.TrimSpace(signals["X-Codex-"+window+"-Window-Minutes"]), 10, 64)
		if minutes >= longestMinutes {
			longestMinutes, resetAt = minutes, at
		}
	}
	if resetAt.IsZero() {
		for _, key := range []string{
			"Anthropic-Ratelimit-Unified-7d-Reset",
			"Anthropic-Ratelimit-Unified-5h-Reset",
			"weekly_quota_reset_at",
			"daily_quota_reset_at",
		} {
			if at, ok := parseQuotaResetTime(signals[key]); ok {
				resetAt = at
				break
			}
		}
	}
	if !resetAt.After(now) {
		return time.Time{}, false
	}
	return resetAt, true
}

func codexWindowResetAt(quota QuotaState, prefix string) (time.Time, bool) {
	if at, ok := parseQuotaResetTime(quota.Signals[prefix+"Reset-At"]); ok {
		return at, true
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(quota.Signals[prefix+"Reset-After-Seconds"]), 10, 64)
	if err != nil || seconds < 0 || quota.ObservedAt.IsZero() {
		return time.Time{}, false
	}
	return quota.ObservedAt.Add(time.Duration(seconds) * time.Second), true
}

// parseQuotaResetTime accepts Unix seconds, Unix milliseconds, or RFC 3339.
func parseQuotaResetTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if value <= 0 {
			return time.Time{}, false
		}
		if value > 1_000_000_000_000 {
			return time.UnixMilli(value), true
		}
		return time.Unix(value, 0), true
	}
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at, true
	}
	return time.Time{}, false
}
