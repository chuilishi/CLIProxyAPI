// Package auth provides load-aware account selection for optimal request distribution.
// This file implements a selector that tracks account busyness (inflight requests and latency)
// to prioritize less busy accounts, improving overall response times.
package auth

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// LoadAwareSelector selects credentials based on current load (inflight requests and latency).
// It prioritizes accounts with fewer active requests and lower average latency.
//
// 设计理念：
// - 追踪每个账号的当前并发请求数 (inflight)
// - 追踪每个账号的近期平均响应延迟
// - 选择账号时优先选择负载较低的账号
// - 结合配额信息进行综合评分
type LoadAwareSelector struct {
	mu sync.RWMutex

	// 每个账号的负载状态
	loadStats map[string]*AccountLoadStats

	// 配额感知选择器（可选，用于结合配额信息）
	quotaSelector *QuotaAwareSelector

	// Configuration
	LatencyWindowSize   int           // 延迟采样窗口大小 (默认: 10)
	LatencyWeight       float64       // 延迟在评分中的权重 (默认: 0.3)
	InflightWeight      float64       // 并发数在评分中的权重 (默认: 0.5)
	QuotaWeight         float64       // 配额在评分中的权重 (默认: 0.2)
	MaxInflightPerAuth  int           // 单账号最大并发数警告阈值 (默认: 5)
	LatencyDecayFactor  float64       // 延迟衰减因子，越新的样本权重越高 (默认: 0.9)
	StatsExpiry         time.Duration // 统计数据过期时间 (默认: 30min)
}

// AccountLoadStats tracks load statistics for a single account.
type AccountLoadStats struct {
	AuthID string

	// 当前并发请求数
	Inflight atomic.Int64

	// 延迟追踪
	latencies   []latencySample
	latencyLock sync.RWMutex

	// 统计信息
	TotalRequests   atomic.Int64
	SuccessRequests atomic.Int64
	FailedRequests  atomic.Int64

	// 最后活动时间
	LastActive time.Time
}

// latencySample records a single latency measurement.
type latencySample struct {
	Duration  time.Duration
	Timestamp time.Time
}

// NewLoadAwareSelector creates a new load-aware selector with default settings.
func NewLoadAwareSelector() *LoadAwareSelector {
	return &LoadAwareSelector{
		loadStats:          make(map[string]*AccountLoadStats),
		quotaSelector:      NewQuotaAwareSelector(),
		LatencyWindowSize:  10,
		LatencyWeight:      0.3,
		InflightWeight:     0.5,
		QuotaWeight:        0.2,
		MaxInflightPerAuth: 5,
		LatencyDecayFactor: 0.9,
		StatsExpiry:        30 * time.Minute,
	}
}

// WithQuotaSelector sets a custom quota selector.
func (s *LoadAwareSelector) WithQuotaSelector(qs *QuotaAwareSelector) *LoadAwareSelector {
	s.quotaSelector = qs
	return s
}

// Pick selects the auth with the lowest load score.
func (s *LoadAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	if len(available) == 1 {
		return available[0], nil
	}

	// 计算每个账号的综合负载评分
	type scoredAuth struct {
		auth  *Auth
		score float64 // 分数越低越好
	}

	scored := make([]scoredAuth, 0, len(available))
	for _, auth := range available {
		score := s.calculateLoadScore(auth, model)
		scored = append(scored, scoredAuth{auth: auth, score: score})
	}

	// 按分数升序排序（分数越低越好）
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	best := scored[0]

	// 记录选择日志
	stats := s.getOrCreateStats(best.auth.ID)
	inflight := stats.Inflight.Load()
	if inflight >= int64(s.MaxInflightPerAuth) {
		log.Warnf("[LoadAwareSelector] selected account %s has high inflight: %d", best.auth.ID, inflight)
	} else {
		log.Debugf("[LoadAwareSelector] selected account %s (score=%.2f, inflight=%d)", best.auth.ID, best.score, inflight)
	}

	return best.auth, nil
}

// calculateLoadScore calculates a composite load score for an auth.
// Lower score = better (less busy, lower latency, more quota).
func (s *LoadAwareSelector) calculateLoadScore(auth *Auth, model string) float64 {
	stats := s.getOrCreateStats(auth.ID)

	// 1. 并发请求分数 (0-100, 越高越忙)
	inflight := stats.Inflight.Load()
	inflightScore := float64(inflight) * 20.0 // 每个并发请求增加20分
	if inflightScore > 100 {
		inflightScore = 100
	}

	// 2. 延迟分数 (0-100, 越高越慢)
	avgLatency := s.getAverageLatency(stats)
	latencyScore := 0.0
	if avgLatency > 0 {
		// 将延迟转换为分数：1秒=10分，10秒=100分
		latencyScore = float64(avgLatency.Milliseconds()) / 100.0
		if latencyScore > 100 {
			latencyScore = 100
		}
	}

	// 3. 配额分数 (0-100, 越高配额越少)
	quotaScore := 0.0
	if s.quotaSelector != nil {
		quota := s.quotaSelector.getQuotaForAuth(auth, model)
		// 配额 100% -> 0分, 配额 0% -> 100分
		quotaScore = 100.0 - quota
	}

	// 综合评分
	totalScore := inflightScore*s.InflightWeight +
		latencyScore*s.LatencyWeight +
		quotaScore*s.QuotaWeight

	return totalScore
}

// getAverageLatency calculates the weighted average latency for an account.
func (s *LoadAwareSelector) getAverageLatency(stats *AccountLoadStats) time.Duration {
	stats.latencyLock.RLock()
	defer stats.latencyLock.RUnlock()

	if len(stats.latencies) == 0 {
		return 0
	}

	// 加权平均，越新的样本权重越高
	var totalWeight float64
	var weightedSum float64
	weight := 1.0

	// 从最新到最旧遍历
	for i := len(stats.latencies) - 1; i >= 0; i-- {
		sample := stats.latencies[i]
		weightedSum += float64(sample.Duration.Milliseconds()) * weight
		totalWeight += weight
		weight *= s.LatencyDecayFactor
	}

	if totalWeight == 0 {
		return 0
	}

	avgMs := weightedSum / totalWeight
	return time.Duration(avgMs) * time.Millisecond
}

// getOrCreateStats gets or creates load stats for an auth.
func (s *LoadAwareSelector) getOrCreateStats(authID string) *AccountLoadStats {
	s.mu.RLock()
	stats, ok := s.loadStats[authID]
	s.mu.RUnlock()

	if ok {
		return stats
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock
	if stats, ok = s.loadStats[authID]; ok {
		return stats
	}

	stats = &AccountLoadStats{
		AuthID:     authID,
		latencies:  make([]latencySample, 0, s.LatencyWindowSize),
		LastActive: time.Now(),
	}
	s.loadStats[authID] = stats
	return stats
}

// OnRequestStart should be called when a request starts.
// Returns a function to call when the request ends.
func (s *LoadAwareSelector) OnRequestStart(authID string) func(success bool) {
	if authID == "" {
		return func(success bool) {}
	}

	stats := s.getOrCreateStats(authID)
	stats.Inflight.Add(1)
	stats.TotalRequests.Add(1)
	startTime := time.Now()

	return func(success bool) {
		stats.Inflight.Add(-1)
		stats.LastActive = time.Now()

		if success {
			stats.SuccessRequests.Add(1)
		} else {
			stats.FailedRequests.Add(1)
		}

		// 记录延迟
		latency := time.Since(startTime)
		s.recordLatency(stats, latency)
	}
}

// recordLatency records a latency sample.
func (s *LoadAwareSelector) recordLatency(stats *AccountLoadStats, latency time.Duration) {
	stats.latencyLock.Lock()
	defer stats.latencyLock.Unlock()

	sample := latencySample{
		Duration:  latency,
		Timestamp: time.Now(),
	}

	// 保持窗口大小
	if len(stats.latencies) >= s.LatencyWindowSize {
		// 移除最旧的样本
		stats.latencies = stats.latencies[1:]
	}
	stats.latencies = append(stats.latencies, sample)
}

// GetInflight returns the current inflight count for an auth.
func (s *LoadAwareSelector) GetInflight(authID string) int64 {
	s.mu.RLock()
	stats, ok := s.loadStats[authID]
	s.mu.RUnlock()

	if !ok {
		return 0
	}
	return stats.Inflight.Load()
}

// GetStats returns load statistics for all accounts.
func (s *LoadAwareSelector) GetStats() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(s.loadStats))
	for _, stats := range s.loadStats {
		avgLatency := s.getAverageLatency(stats)
		stat := map[string]interface{}{
			"auth_id":          stats.AuthID,
			"inflight":         stats.Inflight.Load(),
			"total_requests":   stats.TotalRequests.Load(),
			"success_requests": stats.SuccessRequests.Load(),
			"failed_requests":  stats.FailedRequests.Load(),
			"avg_latency_ms":   avgLatency.Milliseconds(),
			"last_active":      stats.LastActive,
		}
		result = append(result, stat)
	}
	return result
}

// CleanupExpired removes expired stats entries.
func (s *LoadAwareSelector) CleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for authID, stats := range s.loadStats {
		if now.Sub(stats.LastActive) > s.StatsExpiry && stats.Inflight.Load() == 0 {
			delete(s.loadStats, authID)
			log.Debugf("[LoadAwareSelector] cleaned up expired stats for %s", authID)
		}
	}
}

// OnRateLimited delegates to the quota selector if available.
func (s *LoadAwareSelector) OnRateLimited(auth *Auth, model string) {
	if s.quotaSelector != nil {
		s.quotaSelector.OnRateLimited(auth, model)
	}
}

// MarkAuthForbidden delegates to the quota selector if available.
func (s *LoadAwareSelector) MarkAuthForbidden(authID string) {
	if s.quotaSelector != nil {
		s.quotaSelector.MarkAuthForbidden(authID)
	}
}

// UpdateQuotaFromResponse delegates to the quota selector if available.
func (s *LoadAwareSelector) UpdateQuotaFromResponse(authID string, model string, remainingPercent float64, resetAt time.Time) {
	if s.quotaSelector != nil {
		s.quotaSelector.UpdateQuotaFromResponse(authID, model, remainingPercent, resetAt)
	}
}

// GetQuotaStats returns quota statistics from the underlying quota selector.
func (s *LoadAwareSelector) GetQuotaStats() []map[string]interface{} {
	if s.quotaSelector != nil {
		return s.quotaSelector.GetQuotaStats()
	}
	return nil
}
