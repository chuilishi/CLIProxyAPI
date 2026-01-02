# CLIProxyAPI 自定义 Antigravity 同步与智能轮询

## 概述

本目录包含用于同步官方 CLIProxyAPI 仓库并保留自定义 Antigravity 轮询逻辑的工具。

## 文件说明

### 脚本文件
- `sync-and-build.ps1` - 主同步和编译脚本

### 自定义模块 (会被脚本自动保留)
- `internal/runtime/executor/antigravity_smart_retry.go` - 智能重试逻辑
- `internal/runtime/executor/antigravity_account_rotator.go` - 账号轮询器 (executor层)
- `internal/runtime/executor/antigravity_integration.go` - 风控集成模块
- `sdk/cliproxy/auth/quota_aware_selector.go` - 配额感知账号选择器
- `custom/antigravity_config.yaml` - 配置文件

## 使用方法

### 基本同步和编译
```powershell
.\sync-and-build.ps1
```

### 仅同步，不编译
```powershell
.\sync-and-build.ps1 -SkipBuild
```

### 强制同步 (忽略本地未提交修改)
```powershell
.\sync-and-build.ps1 -Force
```

### 干跑模式 (查看将执行的操作)
```powershell
.\sync-and-build.ps1 -DryRun
```

## 智能轮询逻辑说明

### 为什么不用指数退避？

原项目的指数退避 (1s -> 2s -> 4s -> 8s -> ...) 不适合 Antigravity，因为：
1. Antigravity 配额每 **5 小时** 自动刷新
2. 指数退避适合短暂的临时故障，不适合固定周期的配额限制
3. 等待 32 秒后放弃 vs 等待配额恢复，后者更实用

### 智能轮询策略

本模块实现的策略：

1. **解析服务器建议的等待时间** - 从 429 响应中提取 `retryDelay` 或 `quotaResetDelay`
2. **配额感知等待** - 根据配额重置时间智能等待
3. **线性增长而非指数** - 30s -> 45s -> 60s -> ... (最大5分钟)
4. **主动轮询配额** - 等待期间检查配额是否提前恢复
5. **多账号智能切换** - 自动选择配额最多的账号

### 配置项 (custom/antigravity_config.yaml)

```yaml
smartRetry:
  enabled: true              # 启用智能重试
  maxRetries: 10             # 最大重试次数
  initialWaitSeconds: 30     # 初始等待时间
  maxWaitSeconds: 300        # 最大等待时间 (5分钟)
  quotaCheckIntervalSeconds: 60  # 配额检查间隔
  quotaLowThreshold: 5.0     # 配额低阈值 (%)
  checkQuotaWhileWaiting: true   # 等待时检查配额
  quotaResetHours: 5         # 配额刷新周期

accountRotation:
  enabled: true              # 启用账号轮询
  strategy: "quota-aware"    # 策略: round-robin, quota-aware, random
  switchThreshold: 10.0      # 切换阈值 (%)
```

## 关于 Git 冲突

### 这些自定义文件会和官方代码冲突吗？

**不会冲突**，因为：
1. 自定义文件是**新增文件**，不修改官方现有代码
2. `sync-and-build.ps1` 在 `git pull` 前后会自动备份和恢复这些文件
3. 自定义模块通过独立的函数和类型提供功能，可选择性集成

### 如何集成到 executor？

如果你想让智能重试生效，需要在 `antigravity_executor.go` 中调用这些函数。
但这会导致与官方代码冲突。**推荐方案**：

1. **保持独立** - 这些模块作为可用的工具库存在
2. **通过 API 调用** - 外部程序可以调用配额查询 API
3. **Fork 官方仓库** - 如果需要深度集成，建议维护自己的 fork

## 编译输出

默认输出路径: `D:\CliProxyAPI\cli-proxy-api.exe`

可在 `sync-and-build.ps1` 中修改 `$OutputPath` 变量。

## 注意事项

1. 需要安装 Go 1.24+ 和 Git
2. 首次运行会下载依赖，可能需要几分钟
3. 确保网络可以访问 GitHub 和 Go 模块代理
