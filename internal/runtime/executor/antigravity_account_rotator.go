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
}

// DefaultAccountRotationConfig 返回默认配置
func DefaultAccountRotationConfig() AccountRotationConfig {
	return AccountRotationConfig{
		Enabled:         true,
		Strategy:        StrategyQuotaAware,
		SwitchThreshold: 10.0,
	}
}

// AccountQuotaInfo 账号配额信息
type AccountQuotaInfo struct {
	Auth        *cliproxyauth.Auth
	QuotaStatus *QuotaStatus
	LastChecked time.Time
	IsHealthy   bool
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

// selectByQuota 根据配额选择账号
func (r *AccountRotator) selectByQuota(ctx context.Context, modelName string) *cliproxyauth.Auth {
	r.lock.Lock()
	defer r.lock.Unlock()

	// 找到配额最多的健康账号
	type accountScore struct {
		info  *AccountQuotaInfo
		quota float64
	}

	getQuota := func(acc *AccountQuotaInfo) float64 {
		if acc.QuotaStatus == nil {
			return -1 // 未知配额，需要刷新
		}
		// 查找指定模型的配额，或使用平均值
		if q, ok := acc.QuotaStatus.ModelQuotas[modelName]; ok {
			return q
		}
		// 计算平均配额
		total := 0.0
		count := 0
		for _, q := range acc.QuotaStatus.ModelQuotas {
			total += q
			count++
		}
		if count > 0 {
			return total / float64(count)
		}
		return 0
	}

	var candidates []accountScore
	needRefresh := false

	for _, acc := range r.accounts {
		if !acc.IsHealthy {
			continue
		}

		quota := getQuota(acc)
		if quota < 0 {
			// 有账号从未检查过配额，需要刷新
			needRefresh = true
		}
		candidates = append(candidates, accountScore{info: acc, quota: quota})
	}

	// 检查是否所有账号配额都不足
	if !needRefresh && len(candidates) > 0 {
		allLow := true
		for _, c := range candidates {
			if c.quota >= r.config.SwitchThreshold {
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

		// 重新计算配额
		candidates = candidates[:0]
		for _, acc := range r.accounts {
			if !acc.IsHealthy {
				continue
			}
			quota := getQuota(acc)
			candidates = append(candidates, accountScore{info: acc, quota: quota})
		}
	}

	if len(candidates) == 0 {
		// 没有健康账号，返回第一个
		if len(r.accounts) > 0 {
			return r.accounts[0].Auth
		}
		return nil
	}

	// 按配额排序
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].quota > candidates[j].quota
	})

	best := candidates[0]

	// 如果最佳账号配额低于阈值，记录警告
	if best.quota < r.config.SwitchThreshold {
		log.Warnf("account rotator: best account %s has low quota: %.1f%%", best.info.Auth.ID, best.quota)
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
			"id":          acc.Auth.ID,
			"label":       acc.Auth.Label,
			"isHealthy":   acc.IsHealthy,
			"lastChecked": acc.LastChecked,
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
