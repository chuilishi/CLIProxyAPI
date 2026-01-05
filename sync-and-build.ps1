# CLIProxyAPI 一键同步编译脚本
# 功能: 同步上游 -> 编译 -> 重启服务  -> 复制相关文件到执行文件文件夹

$ErrorActionPreference = "Stop"

# ============ 配置 ============
$ProjectRoot = $PSScriptRoot
$OutputPath = "D:\CliProxyAPI\cli-proxy-api.exe"
$UpstreamRemote = "upstream"
$OriginRemote = "origin"
$MainBranch = "main"

# ============ 颜色输出 ============
function Write-Info { param($msg) Write-Host "[INFO] $msg" -ForegroundColor Cyan }
function Write-Success { param($msg) Write-Host "[OK] $msg" -ForegroundColor Green }
function Write-Warn { param($msg) Write-Host "[WARN] $msg" -ForegroundColor Yellow }
function Write-Err { param($msg) Write-Host "[ERROR] $msg" -ForegroundColor Red }

# ============ 显示标题 ============
Write-Host ""
Write-Host "=========================================" -ForegroundColor Magenta
Write-Host "  CLIProxyAPI 一键同步编译" -ForegroundColor Magenta
Write-Host "=========================================" -ForegroundColor Magenta
Write-Host ""

# ============ 检查前置条件 ============
Write-Info "检查环境..."
if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Write-Err "未找到 git"; exit 1 }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Write-Err "未找到 go"; exit 1 }

Push-Location $ProjectRoot

try {
    # ============ 1. 同步上游仓库 ============
    Write-Info "从上游仓库拉取更新..."

    # 检查 upstream remote 是否存在
    $remotes = git remote
    if ($remotes -notcontains $UpstreamRemote) {
        Write-Warn "未找到 upstream remote，添加中..."
        git remote add upstream "https://github.com/router-for-me/CLIProxyAPI.git"
    }

    # Fetch upstream
    git fetch $UpstreamRemote 2>&1 | Out-Null

    # 检查 upstream 是否有新的 commit 需要合并（而不是简单比较是否相同）
    $newCommits = git rev-list "HEAD..$UpstreamRemote/$MainBranch" --count 2>$null

    if ($newCommits -gt 0) {
        Write-Info "发现上游更新 ($newCommits 个新提交)，合并中..."

        # Stash 本地修改（包括暂存区）
        $hasChanges = git status --porcelain
        if ($hasChanges) {
            Write-Info "暂存本地修改..."
            # --keep-index 保留暂存区，--include-untracked 包含新文件
            git stash push -m "auto-stash" --include-untracked 2>&1 | Out-Null
        }

        # 合并上游
        $mergeResult = git merge "$UpstreamRemote/$MainBranch" --no-edit 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Err "合并失败，正在回滚..."
            git merge --abort 2>&1 | Out-Null
            if ($hasChanges) {
                git stash pop --index 2>&1 | Out-Null  # --index 恢复暂存区状态
            }
            Write-Err "请手动解决冲突后重试"
            exit 1
        }

        # 恢复 stash（--index 保留暂存区状态）
        if ($hasChanges) {
            Write-Info "恢复本地修改..."
            $popResult = git stash pop --index 2>&1
            if ($LASTEXITCODE -ne 0) {
                # --index 失败时尝试普通 pop
                Write-Warn "暂存区状态可能已改变，尝试普通恢复..."
                git stash pop 2>&1 | Out-Null
            }
        }

        Write-Success "上游代码已合并"
    }
    else {
        Write-Success "已是最新版本"
    }

    # ============ 2. 停止旧进程（编译前必须停止，否则文件被锁定无法覆盖） ============
    $processName = "cli-proxy-api"
    $existingProcess = Get-Process -Name $processName -ErrorAction SilentlyContinue

    if ($existingProcess) {
        Write-Info "停止旧进程 (PID: $($existingProcess.Id))..."
        Stop-Process -Name $processName -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 1
    }

    # 也停止旧的托盘 PowerShell 进程
    Get-Process -Name "powershell" -ErrorAction SilentlyContinue | Where-Object {
        $_.MainWindowTitle -eq "" -and $_.CommandLine -match "CLIProxyAPI-Tray"
    } | Stop-Process -Force -ErrorAction SilentlyContinue

    # ============ 3. 编译项目 ============
    Write-Info "编译项目..."

    # 确保输出目录存在
    $OutputDir = Split-Path -Parent $OutputPath
    if (-not (Test-Path $OutputDir)) {
        New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
    }

    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"

    $version = git describe --tags --always 2>$null
    if (-not $version) { $version = git rev-parse --short HEAD }

    $ldflags = "-s -w -X 'main.Version=$version'"
    $buildStart = Get-Date

    & go build -ldflags $ldflags -o $OutputPath ./cmd/server/

    if ($LASTEXITCODE -ne 0) {
        Write-Err "编译失败"
        exit 1
    }

    $buildTime = [math]::Round(((Get-Date) - $buildStart).TotalSeconds, 1)
    $sizeMB = [math]::Round((Get-Item $OutputPath).Length / 1MB, 2)
    Write-Success "编译成功 ($sizeMB MB, ${buildTime}s)"

    # 复制附属脚本到输出目录
    $ScriptsSource = Join-Path $ProjectRoot "这下面的文件放到CLIProxyAPI的可执行文件夹里去"
    if (Test-Path $ScriptsSource) {
        $scriptFiles = Get-ChildItem -Path $ScriptsSource -File
        if ($scriptFiles) {
            Write-Info "复制附属脚本到输出目录..."
            foreach ($file in $scriptFiles) {
                Copy-Item -Path $file.FullName -Destination $OutputDir -Force
            }
            Write-Success "已复制 $($scriptFiles.Count) 个脚本文件"
        }
    }

    # ============ 4. 启动服务 ============
    Write-Info "启动服务..."

    # 通过 VBS 静默启动托盘版本
    $vbsPath = Join-Path $OutputDir "CLIProxyAPI-Silent.vbs"
    if (Test-Path $vbsPath) {
        & wscript.exe $vbsPath
        Start-Sleep -Seconds 5  # VBS→PowerShell→exe 启动链需要更多时间
        $newProcess = Get-Process -Name $processName -ErrorAction SilentlyContinue
        if ($newProcess) {
            Write-Success "服务已启动 (PID: $($newProcess.Id)) [托盘模式]"
        }
        else {
            Write-Warn "服务可能启动失败，请检查"
        }
    }
    else {
        # 备用：直接静默启动
        $WshShell = New-Object -ComObject WScript.Shell
        $WshShell.Run("`"$OutputPath`"", 0, $false)
        Start-Sleep -Seconds 2
        $newProcess = Get-Process -Name $processName -ErrorAction SilentlyContinue
        if ($newProcess) {
            Write-Success "服务已启动 (PID: $($newProcess.Id))"
        }
        else {
            Write-Warn "服务可能启动失败，请检查"
        }
    }

    # # ============ 5. 推送到 Fork ============
    # Write-Info "推送到 fork..."

    # $pushResult = git push $OriginRemote $MainBranch 2>&1
    # if ($LASTEXITCODE -eq 0) {
    #     Write-Success "已推送到 fork"
    # }
    # else {
    #     # 可能是没有新提交需要推送
    #     if ($pushResult -match "Everything up-to-date") {
    #         Write-Success "Fork 已是最新"
    #     }
    #     else {
    #         Write-Warn "推送失败: $pushResult"
    #     }
    # }

    # ============ 完成 ============
    Write-Host ""
    Write-Host "=========================================" -ForegroundColor Green
    Write-Host "  全部完成!" -ForegroundColor Green
    Write-Host "=========================================" -ForegroundColor Green
    Write-Host ""

}
finally {
    Pop-Location
}
