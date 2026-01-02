// Package auth provides quota-aware account selection for Antigravity.
// This file implements a selector that prioritizes accounts with highest remaining quota.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// QuotaAwareSelector selects credentials based on remaining quota.
// Prioritizes accounts with highest remaining quota to maximize usage
// and avoid hitting rate limits.
//
// 性能优化：
// - 配额查询是异步的，不阻塞主请求
// - 只在遇到 429 时才主动刷新配额
// - 正常请求使用缓存或估算值
type QuotaAwareSelector struct {
	mu          sync.Mutex
	quotaCache  map[string]*cachedQuota
	lastRefresh time.Time
	refreshLock sync.Mutex

	// 后台刷新状态
	refreshing   bool
	refreshingMu sync.Mutex

	// Configuration
	CacheExpiry       time.Duration // How long quota cache is valid (default: 5min)
	MinQuotaThreshold float64       // Minimum quota to consider account usable (default: 5%)
	EnableAPICheck    bool          // Whether to query Antigravity API for real quota (default: false, only on 429)
}

// cachedQuota stores quota information for an auth
type cachedQuota struct {
	AuthID       string
	ModelQuotas  map[string]float64 // model -> remaining percentage (0-100)
	IsForbidden  bool
	LastChecked  time.Time
	NextResetAt  time.Time
	SubscTier    string // FREE, PRO, ULTRA
}

// antigravityQuotaResponse represents the response from Antigravity quota API
type antigravityQuotaResponse struct {
	Models []struct {
		ModelID          string  `json:"modelId"`
		RemainingPercent float64 `json:"remainingPercent,omitempty"`
		ResetTime        string  `json:"resetTime,omitempty"`
	} `json:"models"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewQuotaAwareSelector creates a new quota-aware selector with default settings.
func NewQuotaAwareSelector() *QuotaAwareSelector {
	return &QuotaAwareSelector{
		quotaCache:        make(map[string]*cachedQuota),
		CacheExpiry:       5 * time.Minute, // 延长到 5 分钟
		MinQuotaThreshold: 5.0,
		EnableAPICheck:    false, // 默认不主动查询，只在 429 时查询
	}
}

// Pick selects the auth with the highest remaining quota.
func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	if len(available) == 1 {
		return available[0], nil
	}

	// 异步刷新配额缓存（不阻塞主请求）
	// 只有开启了 EnableAPICheck 且缓存过期时才触发
	if s.EnableAPICheck && time.Since(s.lastRefresh) > s.CacheExpiry {
		s.triggerAsyncRefresh(available)
	}

	// Score each auth by quota (使用缓存或估算值，不等待 API)
	type scoredAuth struct {
		auth  *Auth
		quota float64
	}

	scored := make([]scoredAuth, 0, len(available))
	for _, auth := range available {
		quota := s.getQuotaForAuth(auth, model)
		scored = append(scored, scoredAuth{auth: auth, quota: quota})
	}

	// Sort by quota descending
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].quota > scored[j].quota
	})

	best := scored[0]

	// Log warning if best account has low quota
	if best.quota < s.MinQuotaThreshold && best.quota > 0 {
		log.Warnf("[QuotaAwareSelector] best account %s has low quota: %.1f%%", best.auth.ID, best.quota)
	}

	return best.auth, nil
}

// triggerAsyncRefresh 异步触发配额刷新（不阻塞）
func (s *QuotaAwareSelector) triggerAsyncRefresh(auths []*Auth) {
	s.refreshingMu.Lock()
	if s.refreshing {
		s.refreshingMu.Unlock()
		return // 已有刷新任务在运行
	}
	s.refreshing = true
	s.refreshingMu.Unlock()

	// 后台异步刷新
	go func() {
		defer func() {
			s.refreshingMu.Lock()
			s.refreshing = false
			s.refreshingMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		s.refreshQuotaCache(ctx, auths)
	}()
}

// getQuotaForAuth returns the quota for an auth, using cache or estimating from state.
func (s *QuotaAwareSelector) getQuotaForAuth(auth *Auth, model string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check cache first
	if cached, ok := s.quotaCache[auth.ID]; ok {
		if time.Since(cached.LastChecked) < s.CacheExpiry {
			if cached.IsForbidden {
				return 0
			}
			if q, ok := cached.ModelQuotas[model]; ok {
				return q
			}
			// Return average if model not found
			total := 0.0
			count := 0
			for _, q := range cached.ModelQuotas {
				total += q
				count++
			}
			if count > 0 {
				return total / float64(count)
			}
		}
	}

	// Fallback: estimate from auth state
	return s.estimateQuotaFromState(auth, model)
}

// estimateQuotaFromState estimates quota from auth's runtime state.
func (s *QuotaAwareSelector) estimateQuotaFromState(auth *Auth, model string) float64 {
	now := time.Now()

	// Check if auth is in cooldown
	if auth.Unavailable {
		if auth.Quota.Exceeded {
			// In quota cooldown
			if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
				return 0
			}
		}
	}

	// Check model-specific state
	if model != "" && len(auth.ModelStates) > 0 {
		if state, ok := auth.ModelStates[model]; ok && state != nil {
			if state.Unavailable && state.Quota.Exceeded {
				if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
					return 0
				}
			}
		}
	}

	// No quota issues detected, assume full quota
	return 100.0
}

// refreshQuotaCache refreshes quota information for all available auths.
func (s *QuotaAwareSelector) refreshQuotaCache(ctx context.Context, auths []*Auth) {
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()

	// Double-check after acquiring lock
	if time.Since(s.lastRefresh) < s.CacheExpiry {
		return
	}

	log.Debug("[QuotaAwareSelector] refreshing quota cache...")

	for _, auth := range auths {
		quota, err := s.fetchQuotaFromAPI(ctx, auth)
		if err != nil {
			log.Debugf("[QuotaAwareSelector] failed to fetch quota for %s: %v", auth.ID, err)
			continue
		}

		s.mu.Lock()
		s.quotaCache[auth.ID] = quota
		s.mu.Unlock()
	}

	s.lastRefresh = time.Now()
}

// fetchQuotaFromAPI fetches quota from Antigravity API.
func (s *QuotaAwareSelector) fetchQuotaFromAPI(ctx context.Context, auth *Auth) (*cachedQuota, error) {
	// Get access token from auth metadata
	accessToken := ""
	if auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok {
			accessToken = token
		}
	}

	if accessToken == "" {
		return nil, fmt.Errorf("no access token found")
	}

	// Get project ID from metadata or fetch it
	projectID := ""
	if auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = pid
		}
	}

	if projectID == "" {
		// Try to fetch project ID from loadCodeAssist API
		pid, err := s.fetchProjectID(ctx, accessToken)
		if err != nil {
			log.Debugf("[QuotaAwareSelector] failed to fetch project_id: %v", err)
		} else {
			projectID = pid
			// Cache it in metadata
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["project_id"] = projectID
		}
	}

	if projectID == "" {
		return nil, fmt.Errorf("no project_id available")
	}

	// Fetch quota from Antigravity API
	url := fmt.Sprintf("https://cloudcode-pa.googleapis.com/v1internal/users/-/projects/%s:fetchAvailableModels", projectID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return &cachedQuota{
			AuthID:      auth.ID,
			IsForbidden: true,
			LastChecked: time.Now(),
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var quotaResp antigravityQuotaResponse
	if err := json.Unmarshal(body, &quotaResp); err != nil {
		return nil, err
	}

	if quotaResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", quotaResp.Error.Message)
	}

	cached := &cachedQuota{
		AuthID:      auth.ID,
		ModelQuotas: make(map[string]float64),
		LastChecked: time.Now(),
	}

	for _, m := range quotaResp.Models {
		cached.ModelQuotas[m.ModelID] = m.RemainingPercent
		if m.ResetTime != "" {
			if resetTime, err := time.Parse(time.RFC3339, m.ResetTime); err == nil {
				if cached.NextResetAt.IsZero() || resetTime.Before(cached.NextResetAt) {
					cached.NextResetAt = resetTime
				}
			}
		}
	}

	return cached, nil
}

// fetchProjectID fetches the project ID from loadCodeAssist API.
func (s *QuotaAwareSelector) fetchProjectID(ctx context.Context, accessToken string) (string, error) {
	url := "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loadCodeAssist returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result struct {
		ProjectID        string `json:"projectId"`
		SubscriptionTier string `json:"subscriptionTier"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	return result.ProjectID, nil
}

// UpdateQuotaFromResponse updates cached quota based on API response.
// Call this when you receive a 429 or quota-related response.
func (s *QuotaAwareSelector) UpdateQuotaFromResponse(authID string, model string, remainingPercent float64, resetAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cached, ok := s.quotaCache[authID]
	if !ok {
		cached = &cachedQuota{
			AuthID:      authID,
			ModelQuotas: make(map[string]float64),
		}
		s.quotaCache[authID] = cached
	}

	cached.ModelQuotas[model] = remainingPercent
	cached.LastChecked = time.Now()
	if !resetAt.IsZero() {
		cached.NextResetAt = resetAt
	}
}

// OnRateLimited should be called when a 429 response is received.
// It marks the auth as having low quota and triggers an async refresh.
func (s *QuotaAwareSelector) OnRateLimited(auth *Auth, model string) {
	if auth == nil {
		return
	}

	// 标记该账号的配额为 0
	s.UpdateQuotaFromResponse(auth.ID, model, 0, time.Time{})

	// 异步刷新该账号的配额（获取真实的 reset time）
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		quota, err := s.fetchQuotaFromAPI(ctx, auth)
		if err != nil {
			log.Debugf("[QuotaAwareSelector] failed to refresh quota after 429: %v", err)
			return
		}

		s.mu.Lock()
		s.quotaCache[auth.ID] = quota
		s.mu.Unlock()

		log.Debugf("[QuotaAwareSelector] refreshed quota for %s after 429, reset at: %v", auth.ID, quota.NextResetAt)
	}()
}

// MarkAuthForbidden marks an auth as forbidden (403).
func (s *QuotaAwareSelector) MarkAuthForbidden(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cached, ok := s.quotaCache[authID]; ok {
		cached.IsForbidden = true
	} else {
		s.quotaCache[authID] = &cachedQuota{
			AuthID:      authID,
			IsForbidden: true,
			LastChecked: time.Now(),
		}
	}
}

// GetQuotaStats returns current quota statistics for debugging.
func (s *QuotaAwareSelector) GetQuotaStats() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := make([]map[string]interface{}, 0, len(s.quotaCache))
	for _, cached := range s.quotaCache {
		stat := map[string]interface{}{
			"auth_id":      cached.AuthID,
			"is_forbidden": cached.IsForbidden,
			"last_checked": cached.LastChecked,
			"model_quotas": cached.ModelQuotas,
		}
		if !cached.NextResetAt.IsZero() {
			stat["next_reset_at"] = cached.NextResetAt
		}
		stats = append(stats, stat)
	}

	return stats
}
