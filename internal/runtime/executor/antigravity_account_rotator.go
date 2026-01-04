// Package executor provides quota-aware account selection for Antigravity.
// This file implements smart account rotation based on quota status.
package executor

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// AccountRotationStrategy 账号轮询策略
type AccountRotationStrategy string

const (
	// StrategyRoundRobin 简单轮询
	StrategyRoundRobin AccountRotationStrategy = "round-robin"
	// StrategyQuotaAware 配额感知 (推荐)
	StrategyQuotaAware AccountRotationStrategy = "quota-aware"
	// StrategyRandom 随机选择
	StrategyRandom AccountRotationStrategy = "random"
)

// AccountRotationConfig 账号轮询配置
type AccountRotationConfig struct {
	Enabled         bool                    `yaml:"enabled" json:"enabled"`
	Strategy        AccountRotationStrategy `yaml:"strategy" json:"strategy"`
	SwitchThreshold float64                 `yaml:"switchThreshold" json:"switchThreshold"`

	// 首字节超时配置
	FirstByteTimeoutMs int `yaml:"firstByteTimeoutMs" json:"firstByteTimeoutMs"` // 首字节超时时间 (毫秒)，默认 10000 (10秒)
	TimeoutPenalty     int `yaml:"timeoutPenalty" json:"timeoutPenalty"`         // 超时惩罚权重，默认 20
	TimeoutDecayMinutes int `yaml:"timeoutDecayMinutes" json:"timeoutDecayMinutes"` // 超时惩罚衰减时间 (分钟)，默认 30
}

// DefaultAccountRotationConfig 返回默认配置
func DefaultAccountRotationConfig() AccountRotationConfig {
	return AccountRotationConfig{
		Enabled:             true,
		Strategy:            StrategyQuotaAware,
		SwitchThreshold:     10.0,
		FirstByteTimeoutMs:  10000, // 10秒
		TimeoutPenalty:      20,    // 超时一次扣20分
		TimeoutDecayMinutes: 30,    // 30分钟后惩罚衰减
	}
}

// AccountQuotaInfo 账号配额信息
type AccountQuotaInfo struct {
	Auth        *cliproxyauth.Auth
	QuotaStatus *QuotaStatus
	LastChecked time.Time
	IsHealthy   bool

	// 首字节超时统计
	TotalRequests      int64     // 总请求数
	TimeoutCount       int64     // 首字节超时次数
	LastTimeoutAt      time.Time // 最后一次超时时间
	AvgFirstByteTimeMs float64   // 平均首字节响应时间 (毫秒)
	RecentLatencies    []int64   // 最近的首字节延迟样本 (毫秒，最多保留10个)
}

// AccountRotator 账号轮询器
type AccountRotator struct {
	cfg           *config.Config
	config        AccountRotationConfig
	quotaChecker  *QuotaChecker
	accounts      []*AccountQuotaInfo
	currentIndex  int
	lock          sync.RWMutex
}

// NewAccountRotator 创建账号轮询器
func NewAccountRotator(cfg *config.Config) *AccountRotator {
	return &AccountRotator{
		cfg:          cfg,
		config:       DefaultAccountRotationConfig(),
		quotaChecker: NewQuotaChecker(cfg),
		accounts:     make([]*AccountQuotaInfo, 0),
	}
}

// WithConfig 设置配置
func (r *AccountRotator) WithConfig(config AccountRotationConfig) *AccountRotator {
	r.config = config
	return r
}

// RegisterAccount 注册账号
func (r *AccountRotator) RegisterAccount(auth *cliproxyauth.Auth) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// 检查是否已存在
	for _, acc := range r.accounts {
		if acc.Auth.ID == auth.ID {
			acc.Auth = auth
			return
		}
	}

	r.accounts = append(r.accounts, &AccountQuotaInfo{
		Auth:      auth,
		IsHealthy: true,
	})
}

// SelectBestAccount 选择最佳账号
func (r *AccountRotator) SelectBestAccount(ctx context.Context, modelName string) *cliproxyauth.Auth {
	if !r.config.Enabled || len(r.accounts) == 0 {
		return nil
	}

	switch r.config.Strategy {
	case StrategyQuotaAware:
		return r.selectByQuota(ctx, modelName)
	case StrategyRoundRobin:
		return r.selectByRoundRobin()
	case StrategyRandom:
		return r.selectRandom()
	default:
		return r.selectByRoundRobin()
	}
}

// selectByQuota 根据配额和超时惩罚综合选择账号
func (r *AccountRotator) selectByQuota(ctx context.Context, modelName string) *cliproxyauth.Auth {
	r.lock.Lock()
	defer r.lock.Unlock()

	// 找到综合评分最高的健康账号
	type accountScore struct {
		info  *AccountQuotaInfo
		score float64 // 综合评分 = 配额分数 - 超时惩罚
	}

	var candidates []accountScore
	needRefresh := false

	for _, acc := range r.accounts {
		if !acc.IsHealthy {
			continue
		}

		// 检查是否需要刷新配额
		if acc.QuotaStatus == nil {
			needRefresh = true
		}

		// 计算综合评分
		score := r.calculateAccountScore(acc, modelName)
		candidates = append(candidates, accountScore{info: acc, score: score})
	}

	// 检查是否所有账号配额都不足
	if !needRefresh && len(candidates) > 0 {
		allLow := true
		for _, c := range candidates {
			if c.score >= r.config.SwitchThreshold {
				allLow = false
				break
			}
		}
		if allLow {
			needRefresh = true
		}
	}

	// 只有在需要时才刷新配额（未初始化或全部配额不足）
	if needRefresh {
		log.Debugf("account rotator: refreshing quotas (all accounts low or uninitialized)")
		r.refreshQuotas(ctx)

		// 重新计算评分
		candidates = candidates[:0]
		for _, acc := range r.accounts {
			if !acc.IsHealthy {
				continue
			}
			score := r.calculateAccountScore(acc, modelName)
			candidates = append(candidates, accountScore{info: acc, score: score})
		}
	}

	if len(candidates) == 0 {
		// 没有健康账号，返回第一个
		if len(r.accounts) > 0 {
			return r.accounts[0].Auth
		}
		return nil
	}

	// 按综合评分排序 (评分高的优先)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	best := candidates[0]

	// 如果最佳账号评分低于阈值，记录警告
	if best.score < r.config.SwitchThreshold {
		log.Warnf("account rotator: best account %s has low score: %.1f (quota - timeout penalty)", best.info.Auth.ID, best.score)
	}

	return best.info.Auth
}

// selectByRoundRobin 轮询选择
func (r *AccountRotator) selectByRoundRobin() *cliproxyauth.Auth {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.accounts) == 0 {
		return nil
	}

	// 找下一个健康的账号
	startIndex := r.currentIndex
	for i := 0; i < len(r.accounts); i++ {
		idx := (startIndex + i) % len(r.accounts)
		if r.accounts[idx].IsHealthy {
			r.currentIndex = (idx + 1) % len(r.accounts)
			return r.accounts[idx].Auth
		}
	}

	// 没有健康账号，返回当前账号
	r.currentIndex = (r.currentIndex + 1) % len(r.accounts)
	return r.accounts[startIndex].Auth
}

// selectRandom 随机选择
func (r *AccountRotator) selectRandom() *cliproxyauth.Auth {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if len(r.accounts) == 0 {
		return nil
	}

	// 收集健康账号
	var healthy []*AccountQuotaInfo
	for _, acc := range r.accounts {
		if acc.IsHealthy {
			healthy = append(healthy, acc)
		}
	}

	if len(healthy) == 0 {
		return r.accounts[0].Auth
	}

	// 使用时间作为随机种子
	idx := int(time.Now().UnixNano()) % len(healthy)
	return healthy[idx].Auth
}

// refreshQuotas 刷新所有账号的配额
func (r *AccountRotator) refreshQuotas(ctx context.Context) {
	for _, acc := range r.accounts {
		status, err := r.quotaChecker.CheckQuota(ctx, acc.Auth)
		if err != nil {
			log.Debugf("account rotator: failed to check quota for %s: %v", acc.Auth.ID, err)
			continue
		}

		acc.QuotaStatus = status
		acc.LastChecked = time.Now()
		acc.IsHealthy = !status.IsForbidden
	}
}

// MarkAccountUnhealthy 标记账号为不健康
func (r *AccountRotator) MarkAccountUnhealthy(authID string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			acc.IsHealthy = false
			log.Warnf("account rotator: marked account %s as unhealthy", authID)
			return
		}
	}
}

// MarkAccountHealthy 标记账号为健康
func (r *AccountRotator) MarkAccountHealthy(authID string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			acc.IsHealthy = true
			return
		}
	}
}

// GetAccountStats 获取账号统计
func (r *AccountRotator) GetAccountStats() []map[string]interface{} {
	r.lock.RLock()
	defer r.lock.RUnlock()

	stats := make([]map[string]interface{}, 0, len(r.accounts))
	for _, acc := range r.accounts {
		stat := map[string]interface{}{
			"id":                 acc.Auth.ID,
			"label":              acc.Auth.Label,
			"isHealthy":          acc.IsHealthy,
			"lastChecked":        acc.LastChecked,
			"totalRequests":      acc.TotalRequests,
			"timeoutCount":       acc.TimeoutCount,
			"lastTimeoutAt":      acc.LastTimeoutAt,
			"avgFirstByteTimeMs": acc.AvgFirstByteTimeMs,
		}

		if acc.QuotaStatus != nil {
			stat["quotas"] = acc.QuotaStatus.ModelQuotas
			stat["isForbidden"] = acc.QuotaStatus.IsForbidden
		}

		stats = append(stats, stat)
	}

	return stats
}

// ================================================================
// 全局实例
// ================================================================

var (
	globalAccountRotator     *AccountRotator
	globalAccountRotatorOnce sync.Once
)

// GetAccountRotator 获取全局账号轮询器
func GetAccountRotator(cfg *config.Config) *AccountRotator {
	globalAccountRotatorOnce.Do(func() {
		globalAccountRotator = NewAccountRotator(cfg)
	})
	return globalAccountRotator
}

// ================================================================
// 首字节超时相关方法
// ================================================================

// RecordFirstByteLatency 记录首字节响应延迟
func (r *AccountRotator) RecordFirstByteLatency(authID string, latencyMs int64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			acc.TotalRequests++

			// 更新最近延迟样本 (保留最近10个)
			if acc.RecentLatencies == nil {
				acc.RecentLatencies = make([]int64, 0, 10)
			}
			if len(acc.RecentLatencies) >= 10 {
				acc.RecentLatencies = acc.RecentLatencies[1:]
			}
			acc.RecentLatencies = append(acc.RecentLatencies, latencyMs)

			// 计算平均延迟
			var sum int64
			for _, l := range acc.RecentLatencies {
				sum += l
			}
			acc.AvgFirstByteTimeMs = float64(sum) / float64(len(acc.RecentLatencies))

			log.Debugf("account rotator: recorded latency %dms for account %s (avg: %.1fms)",
				latencyMs, authID, acc.AvgFirstByteTimeMs)
			return
		}
	}
}

// RecordFirstByteTimeout 记录首字节超时事件
func (r *AccountRotator) RecordFirstByteTimeout(authID string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			acc.TotalRequests++
			acc.TimeoutCount++
			acc.LastTimeoutAt = time.Now()

			log.Warnf("account rotator: recorded first byte timeout for account %s (total timeouts: %d)",
				authID, acc.TimeoutCount)
			return
		}
	}
}

// GetTimeoutPenalty 获取账号的超时惩罚分数
// 惩罚分数会随时间衰减
func (r *AccountRotator) GetTimeoutPenalty(authID string) float64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			return r.calculateTimeoutPenalty(acc)
		}
	}
	return 0
}

// calculateTimeoutPenalty 计算超时惩罚分数
func (r *AccountRotator) calculateTimeoutPenalty(acc *AccountQuotaInfo) float64 {
	if acc.TimeoutCount == 0 {
		return 0
	}

	// 基础惩罚 = 超时次数 * 惩罚权重
	basePenalty := float64(acc.TimeoutCount) * float64(r.config.TimeoutPenalty)

	// 计算衰减系数 (根据最后一次超时的时间)
	if acc.LastTimeoutAt.IsZero() {
		return basePenalty
	}

	minutesSinceTimeout := time.Since(acc.LastTimeoutAt).Minutes()
	decayMinutes := float64(r.config.TimeoutDecayMinutes)
	if decayMinutes <= 0 {
		decayMinutes = 30
	}

	// 使用指数衰减: penalty * exp(-t/decay)
	decayFactor := 1.0
	if minutesSinceTimeout > 0 {
		decayFactor = 1.0 / (1.0 + minutesSinceTimeout/decayMinutes)
	}

	return basePenalty * decayFactor
}

// GetFirstByteTimeoutDuration 获取首字节超时时间
func (r *AccountRotator) GetFirstByteTimeoutDuration() time.Duration {
	if r.config.FirstByteTimeoutMs <= 0 {
		return 10 * time.Second // 默认10秒
	}
	return time.Duration(r.config.FirstByteTimeoutMs) * time.Millisecond
}

// GetAccountScore 获取账号综合评分 (配额 - 超时惩罚)
func (r *AccountRotator) GetAccountScore(authID string, modelName string) float64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	for _, acc := range r.accounts {
		if acc.Auth.ID == authID {
			return r.calculateAccountScore(acc, modelName)
		}
	}
	return 0
}

// calculateAccountScore 计算账号综合评分
func (r *AccountRotator) calculateAccountScore(acc *AccountQuotaInfo, modelName string) float64 {
	// 基础分数: 配额百分比 (0-100)
	baseScore := 50.0 // 默认50分 (未知配额)

	if acc.QuotaStatus != nil {
		if q, ok := acc.QuotaStatus.ModelQuotas[modelName]; ok {
			baseScore = q
		} else {
			// 计算平均配额
			total := 0.0
			count := 0
			for _, q := range acc.QuotaStatus.ModelQuotas {
				total += q
				count++
			}
			if count > 0 {
				baseScore = total / float64(count)
			}
		}
	}

	// 扣除超时惩罚
	penalty := r.calculateTimeoutPenalty(acc)
	finalScore := baseScore - penalty

	// 不健康的账号得分为负
	if !acc.IsHealthy {
		finalScore = -100
	}

	return finalScore
}
