// Package executor provides smart retry logic for Antigravity service.
// This file implements intelligent polling based on the 5-hour quota reset cycle.
//
// 设计理念:
// 1. Antigravity 的配额每5小时自动刷新
// 2. 不使用指数退避 (1,2,4,8...) 因为这不适合固定刷新周期的场景
// 3. 使用智能轮询: 根据配额剩余情况和重置时间动态调整等待策略
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// SmartRetryConfig 智能重试配置
type SmartRetryConfig struct {
	// 是否启用智能重试 (默认 true)
	Enabled bool `yaml:"enabled" json:"enabled"`

	// 最大重试次数 (默认 10)
	MaxRetries int `yaml:"maxRetries" json:"maxRetries"`

	// 初始等待时间 (默认 30s)
	InitialWaitSeconds int `yaml:"initialWaitSeconds" json:"initialWaitSeconds"`

	// 最大等待时间 (默认 300s = 5min)
	MaxWaitSeconds int `yaml:"maxWaitSeconds" json:"maxWaitSeconds"`

	// 配额检查间隔 (默认 60s)
	QuotaCheckIntervalSeconds int `yaml:"quotaCheckIntervalSeconds" json:"quotaCheckIntervalSeconds"`

	// 配额阈值：低于此百分比时认为配额不足 (默认 5%)
	QuotaLowThreshold float64 `yaml:"quotaLowThreshold" json:"quotaLowThreshold"`

	// 是否在等待时查询配额状态 (默认 true)
	CheckQuotaWhileWaiting bool `yaml:"checkQuotaWhileWaiting" json:"checkQuotaWhileWaiting"`

	// 配额刷新周期 (默认 5h)
	QuotaResetHours int `yaml:"quotaResetHours" json:"quotaResetHours"`
}

// DefaultSmartRetryConfig 返回默认配置
func DefaultSmartRetryConfig() SmartRetryConfig {
	return SmartRetryConfig{
		Enabled:                   true,
		MaxRetries:                10,
		InitialWaitSeconds:        30,
		MaxWaitSeconds:            300,
		QuotaCheckIntervalSeconds: 60,
		QuotaLowThreshold:         5.0,
		CheckQuotaWhileWaiting:    true,
		QuotaResetHours:           5,
	}
}

// QuotaStatus 配额状态
type QuotaStatus struct {
	// 模型名称 -> 剩余百分比 (0-100)
	ModelQuotas map[string]float64

	// 模型名称 -> 重置时间
	ResetTimes map[string]time.Time

	// 最后更新时间
	LastUpdated time.Time

	// 是否被禁止访问
	IsForbidden bool
}

// SmartRetryManager 智能重试管理器
type SmartRetryManager struct {
	cfg    *config.Config
	config SmartRetryConfig

	// 缓存配额状态
	quotaCache     map[string]*QuotaStatus // authID -> status
	quotaCacheLock sync.RWMutex

	// 异步刷新状态
	refreshing   map[string]bool // authID -> is refreshing
	refreshingMu sync.Mutex

	// 统计
	stats struct {
		totalRetries     int64
		successfulAfter  int64 // 重试后成功的次数
		quotaWaits       int64 // 等待配额恢复的次数
		quotaCheckCalls  int64 // 配额查询调用次数
	}
	statsLock sync.Mutex
}

// NewSmartRetryManager 创建智能重试管理器
func NewSmartRetryManager(cfg *config.Config) *SmartRetryManager {
	return &SmartRetryManager{
		cfg:        cfg,
		config:     DefaultSmartRetryConfig(),
		quotaCache: make(map[string]*QuotaStatus),
		refreshing: make(map[string]bool),
	}
}

// WithConfig 设置配置
func (m *SmartRetryManager) WithConfig(config SmartRetryConfig) *SmartRetryManager {
	m.config = config
	return m
}

// ShouldRetry 判断是否应该重试
func (m *SmartRetryManager) ShouldRetry(ctx context.Context, auth *cliproxyauth.Auth, statusCode int, responseBody []byte, attempt int) (bool, time.Duration) {
	if !m.config.Enabled {
		return false, 0
	}

	if attempt >= m.config.MaxRetries {
		log.Debugf("smart retry: max retries (%d) reached", m.config.MaxRetries)
		return false, 0
	}

	// 只对 429 (Rate Limited) 和部分 5xx 错误重试
	if statusCode != http.StatusTooManyRequests && statusCode < 500 {
		return false, 0
	}

	// 尝试从响应中解析重试延迟
	if delay := m.parseRetryDelay(responseBody); delay > 0 {
		log.Debugf("smart retry: using server-suggested delay: %v", delay)
		return true, m.clampDelay(delay)
	}

	// 计算智能等待时间
	waitDuration := m.calculateWaitDuration(ctx, auth, attempt)
	return true, waitDuration
}

// parseRetryDelay 从响应中解析重试延迟
func (m *SmartRetryManager) parseRetryDelay(body []byte) time.Duration {
	if len(body) == 0 {
		return 0
	}

	bodyStr := string(body)

	// 尝试解析 RetryInfo.retryDelay
	if retryDelay := gjson.Get(bodyStr, "error.details.#(\\@type==\"type.googleapis.com/google.rpc.RetryInfo\").retryDelay"); retryDelay.Exists() {
		if d := m.parseDurationString(retryDelay.String()); d > 0 {
			return d
		}
	}

	// 尝试解析 quotaResetDelay
	if quotaReset := gjson.Get(bodyStr, "error.details.#.metadata.quotaResetDelay"); quotaReset.Exists() {
		for _, item := range quotaReset.Array() {
			if d := m.parseDurationString(item.String()); d > 0 {
				return d
			}
		}
	}

	return 0
}

// parseDurationString 解析持续时间字符串 (如 "1.5s", "200ms", "1h")
func (m *SmartRetryManager) parseDurationString(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// 尝试标准 Go 解析
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}

	// 尝试解析纯数字 (秒)
	var value float64
	var unit string
	if _, err := fmt.Sscanf(s, "%f%s", &value, &unit); err == nil {
		switch strings.ToLower(unit) {
		case "ms":
			return time.Duration(value * float64(time.Millisecond))
		case "s":
			return time.Duration(value * float64(time.Second))
		case "m":
			return time.Duration(value * float64(time.Minute))
		case "h":
			return time.Duration(value * float64(time.Hour))
		}
	}

	return 0
}

// calculateWaitDuration 计算等待时间
func (m *SmartRetryManager) calculateWaitDuration(ctx context.Context, auth *cliproxyauth.Auth, attempt int) time.Duration {
	// 获取配额状态
	quotaStatus := m.getQuotaStatus(ctx, auth)

	// 如果有重置时间信息，使用智能等待
	if quotaStatus != nil && len(quotaStatus.ResetTimes) > 0 {
		return m.calculateQuotaAwareWait(quotaStatus, attempt)
	}

	// 否则使用线性递增策略 (不是指数)
	// 对于 5 小时刷新周期，使用较长的固定间隔更合理
	baseWait := time.Duration(m.config.InitialWaitSeconds) * time.Second
	waitDuration := baseWait * time.Duration(attempt+1)

	return m.clampDelay(waitDuration)
}

// calculateQuotaAwareWait 根据配额信息计算等待时间
func (m *SmartRetryManager) calculateQuotaAwareWait(status *QuotaStatus, attempt int) time.Duration {
	now := time.Now()
	var nearestReset time.Time

	// 找到最近的重置时间
	for _, resetTime := range status.ResetTimes {
		if resetTime.After(now) {
			if nearestReset.IsZero() || resetTime.Before(nearestReset) {
				nearestReset = resetTime
			}
		}
	}

	if nearestReset.IsZero() {
		// 没有有效的重置时间，使用默认策略
		return time.Duration(m.config.InitialWaitSeconds) * time.Second
	}

	timeUntilReset := nearestReset.Sub(now)

	// 如果距离重置时间很近 (< 5分钟)，直接等到重置
	if timeUntilReset <= 5*time.Minute {
		log.Debugf("smart retry: quota reset in %v, waiting until reset", timeUntilReset)
		// 加一点缓冲时间
		return timeUntilReset + 10*time.Second
	}

	// 如果距离重置时间较远，使用分段等待策略
	// 每次等待一定时间后重新检查配额状态
	checkInterval := time.Duration(m.config.QuotaCheckIntervalSeconds) * time.Second

	// 根据重试次数逐渐增加等待时间，但不超过检查间隔
	waitDuration := time.Duration(m.config.InitialWaitSeconds) * time.Second
	waitDuration += time.Duration(attempt) * 15 * time.Second

	if waitDuration > checkInterval {
		waitDuration = checkInterval
	}

	return m.clampDelay(waitDuration)
}

// clampDelay 限制延迟在合理范围内
func (m *SmartRetryManager) clampDelay(d time.Duration) time.Duration {
	minDelay := time.Duration(m.config.InitialWaitSeconds) * time.Second
	maxDelay := time.Duration(m.config.MaxWaitSeconds) * time.Second

	if d < minDelay {
		return minDelay
	}
	if d > maxDelay {
		return maxDelay
	}
	return d
}

// getQuotaStatus 获取配额状态 (带缓存，异步刷新)
func (m *SmartRetryManager) getQuotaStatus(ctx context.Context, auth *cliproxyauth.Auth) *QuotaStatus {
	if auth == nil {
		return nil
	}

	authID := auth.ID
	if authID == "" {
		authID = "default"
	}

	// 检查缓存
	m.quotaCacheLock.RLock()
	cached, exists := m.quotaCache[authID]
	m.quotaCacheLock.RUnlock()

	// 缓存有效期: 5分钟（延长缓存时间）
	if exists && time.Since(cached.LastUpdated) < 5*time.Minute {
		return cached
	}

	// 如果缓存过期，触发异步刷新（不阻塞当前请求）
	if m.config.CheckQuotaWhileWaiting {
		m.triggerAsyncQuotaRefresh(auth)
	}

	// 返回旧缓存（即使过期也返回，避免阻塞）
	return cached
}

// triggerAsyncQuotaRefresh 异步触发配额刷新
func (m *SmartRetryManager) triggerAsyncQuotaRefresh(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}

	authID := auth.ID
	if authID == "" {
		authID = "default"
	}

	// 检查是否已在刷新中
	m.refreshingMu.Lock()
	if m.refreshing[authID] {
		m.refreshingMu.Unlock()
		return
	}
	m.refreshing[authID] = true
	m.refreshingMu.Unlock()

	// 后台异步刷新
	go func() {
		defer func() {
			m.refreshingMu.Lock()
			delete(m.refreshing, authID)
			m.refreshingMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if status := m.fetchQuotaStatus(ctx, auth); status != nil {
			m.quotaCacheLock.Lock()
			m.quotaCache[authID] = status
			m.quotaCacheLock.Unlock()
			log.Debugf("smart retry: async quota refresh completed for %s", authID)
		}
	}()
}

// fetchQuotaStatus 查询配额状态
func (m *SmartRetryManager) fetchQuotaStatus(ctx context.Context, auth *cliproxyauth.Auth) *QuotaStatus {
	if auth == nil {
		return nil
	}

	m.statsLock.Lock()
	m.stats.quotaCheckCalls++
	m.statsLock.Unlock()

	// 获取 access token
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return nil
	}

	// 调用配额 API
	quotaURL := "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, quotaURL, strings.NewReader(`{"project":"auto"}`))
	if err != nil {
		return nil
	}

	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Debugf("smart retry: quota check failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	return m.parseQuotaResponse(body)
}

// parseQuotaResponse 解析配额响应
func (m *SmartRetryManager) parseQuotaResponse(body []byte) *QuotaStatus {
	status := &QuotaStatus{
		ModelQuotas: make(map[string]float64),
		ResetTimes:  make(map[string]time.Time),
		LastUpdated: time.Now(),
	}

	models := gjson.GetBytes(body, "models")
	if !models.Exists() {
		return nil
	}

	models.ForEach(func(key, value gjson.Result) bool {
		modelName := key.String()
		quotaInfo := value.Get("quotaInfo")

		if quotaInfo.Exists() {
			// 解析剩余百分比
			if fraction := quotaInfo.Get("remainingFraction"); fraction.Exists() {
				status.ModelQuotas[modelName] = fraction.Float() * 100
			}

			// 解析重置时间
			if resetTimeStr := quotaInfo.Get("resetTime"); resetTimeStr.Exists() {
				if t, err := time.Parse(time.RFC3339, resetTimeStr.String()); err == nil {
					status.ResetTimes[modelName] = t
				}
			}
		}
		return true
	})

	return status
}

// WaitWithQuotaPolling 等待并轮询配额状态
func (m *SmartRetryManager) WaitWithQuotaPolling(ctx context.Context, auth *cliproxyauth.Auth, totalWait time.Duration) error {
	if !m.config.CheckQuotaWhileWaiting {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(totalWait):
			return nil
		}
	}

	checkInterval := time.Duration(m.config.QuotaCheckIntervalSeconds) * time.Second
	elapsed := time.Duration(0)

	for elapsed < totalWait {
		waitThisRound := checkInterval
		if elapsed+waitThisRound > totalWait {
			waitThisRound = totalWait - elapsed
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitThisRound):
			elapsed += waitThisRound
		}

		// 检查配额是否已恢复
		if elapsed < totalWait {
			status := m.fetchQuotaStatus(ctx, auth)
			if status != nil {
				// 检查是否有足够配额
				hasQuota := false
				for _, quota := range status.ModelQuotas {
					if quota > m.config.QuotaLowThreshold {
						hasQuota = true
						break
					}
				}
				if hasQuota {
					log.Debugf("smart retry: quota recovered early, continuing")
					return nil
				}
			}
		}
	}

	return nil
}

// GetStats 获取统计信息
func (m *SmartRetryManager) GetStats() map[string]int64 {
	m.statsLock.Lock()
	defer m.statsLock.Unlock()

	return map[string]int64{
		"totalRetries":    m.stats.totalRetries,
		"successfulAfter": m.stats.successfulAfter,
		"quotaWaits":      m.stats.quotaWaits,
		"quotaCheckCalls": m.stats.quotaCheckCalls,
	}
}

// RecordRetry 记录重试
func (m *SmartRetryManager) RecordRetry() {
	m.statsLock.Lock()
	m.stats.totalRetries++
	m.statsLock.Unlock()
}

// RecordSuccess 记录成功
func (m *SmartRetryManager) RecordSuccess(afterRetry bool) {
	if afterRetry {
		m.statsLock.Lock()
		m.stats.successfulAfter++
		m.statsLock.Unlock()
	}
}

// ================================================================
// 辅助函数: 计算到下一个配额重置的时间
// ================================================================

// TimeUntilNextQuotaReset 计算距离下一次配额重置的时间
// Antigravity 配额每5小时重置一次
func TimeUntilNextQuotaReset(quotaResetHours int) time.Duration {
	now := time.Now().UTC()

	// 配额重置周期 (默认5小时)
	resetPeriod := time.Duration(quotaResetHours) * time.Hour

	// 计算今天开始到现在的时间
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(startOfDay)

	// 计算下一个重置点
	cyclePosition := elapsed % resetPeriod
	timeUntilReset := resetPeriod - cyclePosition

	return timeUntilReset
}

// FormatDuration 格式化持续时间为人类可读格式
func FormatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// ================================================================
// 全局实例
// ================================================================

var (
	globalSmartRetryManager     *SmartRetryManager
	globalSmartRetryManagerOnce sync.Once
)

// GetSmartRetryManager 获取全局智能重试管理器
func GetSmartRetryManager(cfg *config.Config) *SmartRetryManager {
	globalSmartRetryManagerOnce.Do(func() {
		globalSmartRetryManager = NewSmartRetryManager(cfg)
	})
	return globalSmartRetryManager
}

// ================================================================
// 配额查询工具 (可供外部使用)
// ================================================================

// QuotaChecker 配额检查器
type QuotaChecker struct {
	cfg *config.Config
}

// NewQuotaChecker 创建配额检查器
func NewQuotaChecker(cfg *config.Config) *QuotaChecker {
	return &QuotaChecker{cfg: cfg}
}

// CheckQuota 检查指定账号的配额
func (c *QuotaChecker) CheckQuota(ctx context.Context, auth *cliproxyauth.Auth) (*QuotaStatus, error) {
	if auth == nil {
		return nil, fmt.Errorf("auth is nil")
	}

	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return nil, fmt.Errorf("no access token")
	}

	return c.fetchQuota(ctx, accessToken)
}

// fetchQuota 查询配额
func (c *QuotaChecker) fetchQuota(ctx context.Context, accessToken string) (*QuotaStatus, error) {
	quotaURL := "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

	payload := `{"project":"auto"}`
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, quotaURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", defaultAntigravityAgent)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return &QuotaStatus{IsForbidden: true, LastUpdated: time.Now()}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	return parseQuotaStatusFromBody(body), nil
}

// parseQuotaStatusFromBody 从响应体解析配额状态
func parseQuotaStatusFromBody(body []byte) *QuotaStatus {
	status := &QuotaStatus{
		ModelQuotas: make(map[string]float64),
		ResetTimes:  make(map[string]time.Time),
		LastUpdated: time.Now(),
	}

	models := gjson.GetBytes(body, "models")
	if !models.Exists() {
		return status
	}

	models.ForEach(func(key, value gjson.Result) bool {
		modelName := key.String()

		// 只关心 gemini 和 claude 模型
		if !strings.Contains(modelName, "gemini") && !strings.Contains(modelName, "claude") {
			return true
		}

		quotaInfo := value.Get("quotaInfo")
		if !quotaInfo.Exists() {
			return true
		}

		// 解析剩余百分比
		if fraction := quotaInfo.Get("remainingFraction"); fraction.Exists() {
			status.ModelQuotas[modelName] = math.Round(fraction.Float()*10000) / 100 // 保留2位小数
		}

		// 解析重置时间
		if resetTimeStr := quotaInfo.Get("resetTime"); resetTimeStr.Exists() {
			if t, err := time.Parse(time.RFC3339, resetTimeStr.String()); err == nil {
				status.ResetTimes[modelName] = t
			}
		}

		return true
	})

	return status
}

// QuotaStatusJSON 返回 JSON 格式的配额状态
func (s *QuotaStatus) ToJSON() ([]byte, error) {
	return json.Marshal(s)
}

// Summary 返回配额摘要
func (s *QuotaStatus) Summary() string {
	if s.IsForbidden {
		return "Account is forbidden (403)"
	}

	if len(s.ModelQuotas) == 0 {
		return "No quota information available"
	}

	var sb strings.Builder
	sb.WriteString("Quota Status:\n")

	for model, quota := range s.ModelQuotas {
		resetStr := ""
		if resetTime, ok := s.ResetTimes[model]; ok {
			remaining := time.Until(resetTime)
			if remaining > 0 {
				resetStr = fmt.Sprintf(" (reset in %s)", FormatDuration(remaining))
			} else {
				resetStr = " (reset time passed)"
			}
		}
		sb.WriteString(fmt.Sprintf("  - %s: %.1f%%%s\n", model, quota, resetStr))
	}

	return sb.String()
}
