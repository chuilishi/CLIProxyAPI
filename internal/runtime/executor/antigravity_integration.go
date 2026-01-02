// Package executor provides integration utilities for Antigravity smart retry
// and quota-aware account selection. This file wires together the custom
// antigravity modules with the main executor.
package executor

import (
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// AntigravityRiskMitigation provides anti-risk-control strategies
type AntigravityRiskMitigation struct {
	cfg *config.Config

	// Request interval randomization (disabled by default)
	enableJitter  bool
	minIntervalMs int
	maxIntervalMs int
	lastRequest   time.Time
	requestLock   sync.Mutex

	// Smart retry manager
	retryManager *SmartRetryManager

	// Account stats for monitoring
	requestCounts map[string]int64
	errorCounts   map[string]int64
	statsLock     sync.RWMutex
}

// NewAntigravityRiskMitigation creates a new risk mitigation instance
func NewAntigravityRiskMitigation(cfg *config.Config) *AntigravityRiskMitigation {
	return &AntigravityRiskMitigation{
		cfg:           cfg,
		enableJitter:  false, // 默认禁用抖动，不影响性能
		minIntervalMs: 50,    // 如果启用，最小 50ms
		maxIntervalMs: 200,   // 如果启用，最大 200ms
		retryManager:  GetSmartRetryManager(cfg),
		requestCounts: make(map[string]int64),
		errorCounts:   make(map[string]int64),
	}
}

// EnableRequestJitter enables request jitter with custom intervals
func (m *AntigravityRiskMitigation) EnableRequestJitter(minMs, maxMs int) {
	m.requestLock.Lock()
	defer m.requestLock.Unlock()
	m.enableJitter = true
	m.minIntervalMs = minMs
	m.maxIntervalMs = maxMs
}

// DisableRequestJitter disables request jitter
func (m *AntigravityRiskMitigation) DisableRequestJitter() {
	m.requestLock.Lock()
	defer m.requestLock.Unlock()
	m.enableJitter = false
}

// ApplyRequestJitter adds random delay between requests to appear more human-like
// Only applies if jitter is enabled via EnableRequestJitter()
func (m *AntigravityRiskMitigation) ApplyRequestJitter(ctx context.Context) {
	m.requestLock.Lock()
	defer m.requestLock.Unlock()

	// 如果未启用抖动，直接返回
	if !m.enableJitter {
		return
	}

	// Calculate time since last request
	elapsed := time.Since(m.lastRequest)
	minInterval := time.Duration(m.minIntervalMs) * time.Millisecond

	// If we're sending requests too fast, add jitter
	if elapsed < minInterval {
		jitter := time.Duration(m.minIntervalMs+rand.Intn(m.maxIntervalMs-m.minIntervalMs)) * time.Millisecond
		waitTime := jitter - elapsed
		if waitTime > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(waitTime):
			}
		}
	}

	m.lastRequest = time.Now()
}

// ShouldRetryWithSmartLogic determines if a request should be retried
func (m *AntigravityRiskMitigation) ShouldRetryWithSmartLogic(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	statusCode int,
	responseBody []byte,
	attempt int,
) (shouldRetry bool, waitDuration time.Duration) {
	return m.retryManager.ShouldRetry(ctx, auth, statusCode, responseBody, attempt)
}

// HandleRateLimitResponse processes a 429 response and determines action
func (m *AntigravityRiskMitigation) HandleRateLimitResponse(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	responseBody []byte,
	attempt int,
) (shouldRetry bool, waitDuration time.Duration) {
	shouldRetry, waitDuration = m.retryManager.ShouldRetry(ctx, auth, http.StatusTooManyRequests, responseBody, attempt)

	if shouldRetry {
		m.retryManager.RecordRetry()
		log.Debugf("[RiskMitigation] rate limited, will retry after %v (attempt %d)", waitDuration, attempt+1)
	}

	return shouldRetry, waitDuration
}

// Handle403Forbidden processes a 403 Forbidden response
func (m *AntigravityRiskMitigation) Handle403Forbidden(auth *cliproxyauth.Auth) {
	m.statsLock.Lock()
	m.errorCounts[auth.ID]++
	m.statsLock.Unlock()

	// Update auth status to indicate this account is forbidden
	if auth != nil {
		auth.Unavailable = true
		auth.StatusMessage = "Account forbidden (403)"
		log.Warnf("[RiskMitigation] account %s received 403 Forbidden", auth.ID)
	}
}

// RecordRequest records a request for stats
func (m *AntigravityRiskMitigation) RecordRequest(authID string) {
	m.statsLock.Lock()
	m.requestCounts[authID]++
	m.statsLock.Unlock()
}

// RecordError records an error for stats
func (m *AntigravityRiskMitigation) RecordError(authID string) {
	m.statsLock.Lock()
	m.errorCounts[authID]++
	m.statsLock.Unlock()
}

// GetStats returns current statistics
func (m *AntigravityRiskMitigation) GetStats() map[string]interface{} {
	m.statsLock.RLock()
	defer m.statsLock.RUnlock()

	retryStats := m.retryManager.GetStats()

	return map[string]interface{}{
		"requestCounts": m.requestCounts,
		"errorCounts":   m.errorCounts,
		"retryStats":    retryStats,
	}
}

// WaitForQuotaRecovery waits until quota recovers or context is cancelled
func (m *AntigravityRiskMitigation) WaitForQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth, maxWait time.Duration) error {
	return m.retryManager.WaitWithQuotaPolling(ctx, auth, maxWait)
}

// ================================================================
// Global instance
// ================================================================

var (
	globalRiskMitigation     *AntigravityRiskMitigation
	globalRiskMitigationOnce sync.Once
)

// GetRiskMitigation returns the global risk mitigation instance
func GetRiskMitigation(cfg *config.Config) *AntigravityRiskMitigation {
	globalRiskMitigationOnce.Do(func() {
		globalRiskMitigation = NewAntigravityRiskMitigation(cfg)
	})
	return globalRiskMitigation
}

// ================================================================
// Helper: Enhanced error handling for quota/rate limit responses
// ================================================================

// ParseQuotaErrorDetails extracts quota information from error response
func ParseQuotaErrorDetails(body []byte) *QuotaErrorInfo {
	if len(body) == 0 {
		return nil
	}

	info := &QuotaErrorInfo{}

	// Try to parse retryDelay
	if delay := parseRetryDelayFromBody(body); delay > 0 {
		info.RetryDelay = delay
		info.HasRetryInfo = true
	}

	// Try to parse quota reset time
	if resetTime := parseQuotaResetTimeFromBody(body); !resetTime.IsZero() {
		info.QuotaResetTime = resetTime
		info.HasQuotaInfo = true
	}

	return info
}

// QuotaErrorInfo contains parsed quota error information
type QuotaErrorInfo struct {
	HasRetryInfo   bool
	RetryDelay     time.Duration
	HasQuotaInfo   bool
	QuotaResetTime time.Time
}

// parseRetryDelayFromBody extracts retry delay from response body
func parseRetryDelayFromBody(body []byte) time.Duration {
	mgr := &SmartRetryManager{}
	return mgr.parseRetryDelay(body)
}

// parseQuotaResetTimeFromBody extracts quota reset time from response body
func parseQuotaResetTimeFromBody(body []byte) time.Time {
	// This would parse the resetTime from the response
	// For now, return zero time as the actual parsing depends on response format
	return time.Time{}
}
