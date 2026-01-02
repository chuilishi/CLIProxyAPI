# Claude Code Configuration Script for CLIProxyAPI (Windows)
# This script configures Claude Code to use your local CLIProxyAPI instance
# Run with: powershell -ExecutionPolicy Bypass -File setup-claude-local.ps1
# Or via one-liner: & { $url='http://localhost:8317'; iwr -useb http://localhost:8317/setup-claude-local.ps1 | iex }

param(
    [string]$BaseUrl,
    [string]$ApiKey,
    [switch]$Show,
    [switch]$Help
)

# Check for pre-set variables from one-liner command (use different names to avoid conflict)
if (-not $BaseUrl -and (Test-Path Variable:url)) { $BaseUrl = $url }
if (-not $ApiKey -and (Test-Path Variable:key)) { $ApiKey = $key }

# Configuration - Customized for CLIProxyAPI
$DefaultBaseUrl = "http://localhost:8317"
$DefaultApiKey = "sk-dummy"  # Placeholder for OAuth login (Antigravity)
$ClaudeConfigDir = "$env:USERPROFILE\.claude"
$ClaudeSettingsFile = "$ClaudeConfigDir\settings.json"

# Color functions for output
function Write-Info {
    param([string]$Message)
    Write-Host "[INFO]" -ForegroundColor Blue -NoNewline
    Write-Host " $Message"
}

function Write-Success {
    param([string]$Message)
    Write-Host "[SUCCESS]" -ForegroundColor Green -NoNewline
    Write-Host " $Message"
}

function Write-Warning {
    param([string]$Message)
    Write-Host "[WARNING]" -ForegroundColor Yellow -NoNewline
    Write-Host " $Message"
}

function Write-Error {
    param([string]$Message)
    Write-Host "[ERROR]" -ForegroundColor Red -NoNewline
    Write-Host " $Message"
}

# Function to show help
function Show-Help {
    Write-Host @"
Claude Code Configuration Script for Local API Router (Windows)

Usage: powershell -ExecutionPolicy Bypass -File setup-claude-local.ps1 [OPTIONS]

Options:
  -BaseUrl <URL> Set the CLIProxyAPI base URL (default: $DefaultBaseUrl)
  -ApiKey <KEY>  Set the API key (optional, can be empty for OAuth login)
  -Show          Show current settings and exit
  -Help          Show this help message

Examples:
  .\setup-claude-local.ps1
  .\setup-claude-local.ps1 -BaseUrl http://localhost:8045 -ApiKey sk-your-key
  .\setup-claude-local.ps1 -Show

Quick setup (uses defaults):
  .\setup-claude-local.ps1

PowerShell Execution Policy:
  If you get an execution policy error, run:
  powershell -ExecutionPolicy Bypass -File setup-claude-local.ps1
"@
    exit 0
}

# Function to backup existing settings
function Backup-Settings {
    if (Test-Path $ClaudeSettingsFile) {
        $timestamp = Get-Date -Format "yyyyMMdd_HHmmss"
        $backupFile = "$ClaudeSettingsFile.backup.$timestamp"
        Copy-Item -Path $ClaudeSettingsFile -Destination $backupFile
        Write-Info "Backed up existing settings to: $backupFile"
    }
}

# Function to create settings directory
function New-SettingsDirectory {
    if (-not (Test-Path $ClaudeConfigDir)) {
        New-Item -ItemType Directory -Path $ClaudeConfigDir -Force | Out-Null
        Write-Info "Created Claude configuration directory: $ClaudeConfigDir"
    }
}

# Function to validate API key format (allows empty for OAuth)
function Test-ApiKey {
    param([string]$ApiKey)

    # Allow empty API key for OAuth login
    if ([string]::IsNullOrEmpty($ApiKey)) {
        return $true
    }

    if ($ApiKey -match '^[A-Za-z0-9_-]+$') {
        return $true
    }
    else {
        Write-Error "Invalid API key format. API key should contain only alphanumeric characters, hyphens, and underscores."
        return $false
    }
}

# Function to test API connection
function Test-ApiConnection {
    param(
        [string]$BaseUrl,
        [string]$ApiKey
    )

    Write-Info "Testing API connection..."

    try {
        $headers = @{
            "Content-Type" = "application/json"
            "X-API-Key"    = $ApiKey
        }

        # Use team balance for team URLs; otherwise user balance with legacy fallback.
        $isTeam = $BaseUrl.EndsWith("/team")
        if ($isTeam) {
            $uri = "$BaseUrl/api/v1/team/stats/spending"
            $balanceField = "daily_remaining"
            $balanceLabel = "Daily remaining"
        }
        else {
            $uri = "$BaseUrl/api/v1/user/balance"
            $balanceField = "balance"
            $balanceLabel = "Current balance"
        }

        $response = Invoke-RestMethod -Uri $uri -Method Get -Headers $headers -ErrorAction Stop

        if ($response.$balanceField) {
            Write-Success "API connection successful! ${balanceLabel}: `$($response.$balanceField)"
            return $true
        }
        else {
            Write-Error "API test failed: Invalid response"
            return $false
        }
    }
    catch {
        if ($_.Exception.Response -and $_.Exception.Response.StatusCode -eq 401) {
            Write-Error "API key authentication failed. Please check your API key."
        }
        elseif ($_.Exception.Message -like "*Unable to connect*" -or $_.Exception.Message -like "*could not be resolved*") {
            Write-Error "Cannot connect to API server. Please check the URL and your internet connection."
            Write-Warning "Make sure your local API router is running on $BaseUrl"
        }
        else {
            Write-Error "API test failed: $($_.Exception.Message)"
        }
        return $false
    }
}

# Function to create Claude Code settings
function New-Settings {
    param(
        [string]$BaseUrl,
        [string]$ApiKey
    )

    $settings = @{
        env   = @{
            ANTHROPIC_BASE_URL                       = $BaseUrl
            ANTHROPIC_AUTH_TOKEN                     = $ApiKey
            CLAUDE_CODE_MAX_OUTPUT_TOKENS            = 200000
            DISABLE_TELEMETRY                        = 1
            DISABLE_ERROR_REPORTING                  = 1
            CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = 1
            CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR = 1
            MAX_THINKING_TOKENS                      = 10000
        }
        model = "opus"
    }

    try {
        $json = $settings | ConvertTo-Json -Depth 10
        Set-Content -Path $ClaudeSettingsFile -Value $json -Encoding UTF8
        Write-Success "Claude Code settings written to: $ClaudeSettingsFile"
        return $true
    }
    catch {
        Write-Error "Failed to create settings file: $($_.Exception.Message)"
        return $false
    }
}

# Function to display current settings
function Show-Settings {
    if (Test-Path $ClaudeSettingsFile) {
        Write-Info "Current Claude Code settings:"
        Write-Host "----------------------------------------"
        $settings = Get-Content $ClaudeSettingsFile -Raw | ConvertFrom-Json
        $settings | ConvertTo-Json -Depth 10
        Write-Host "----------------------------------------"
    }
    else {
        Write-Info "No existing Claude Code settings found."
    }

    Write-Host ""
    Write-Info "Current environment variables:"
    Write-Host "----------------------------------------"
    $baseUrl = [Environment]::GetEnvironmentVariable("ANTHROPIC_BASE_URL", [EnvironmentVariableTarget]::User)
    $authToken = [Environment]::GetEnvironmentVariable("ANTHROPIC_AUTH_TOKEN", [EnvironmentVariableTarget]::User)

    if ($baseUrl) {
        Write-Info "ANTHROPIC_BASE_URL: $baseUrl"
    }
    else {
        Write-Info "ANTHROPIC_BASE_URL: (not set)"
    }

    if ($authToken) {
        $maskedToken = if ($authToken.Length -gt 12) {
            "$($authToken.Substring(0, 8))...$($authToken.Substring($authToken.Length - 4))"
        }
        else {
            "$($authToken.Substring(0, [Math]::Min(4, $authToken.Length)))..."
        }
        Write-Info "ANTHROPIC_AUTH_TOKEN: $maskedToken"
    }
    else {
        Write-Info "ANTHROPIC_AUTH_TOKEN: (not set)"
    }
    Write-Host "----------------------------------------"
}

# Main function
function Main {
    Write-Info "Claude Code Local Configuration Script"
    Write-Host "======================================================="
    Write-Host ""

    # Handle command line arguments
    if ($Help) {
        Show-Help
    }

    if ($Show) {
        Show-Settings
        exit 0
    }

    # Use defaults if not provided
    if (-not $BaseUrl) {
        $BaseUrl = $DefaultBaseUrl
        Write-Info "Using default Base URL: $BaseUrl"
    }

    if (-not $ApiKey) {
        $ApiKey = $DefaultApiKey
        if ([string]::IsNullOrEmpty($ApiKey)) {
            Write-Info "No API Key set (using OAuth login)"
        }
        else {
            Write-Info "Using default API Key"
        }
    }

    # Validate API key (skip if empty - using OAuth)
    if (-not [string]::IsNullOrEmpty($ApiKey) -and -not (Test-ApiKey $ApiKey)) {
        exit 1
    }

    # Remove trailing slash from URL
    $BaseUrl = $BaseUrl.TrimEnd('/')

    Write-Host ""
    Write-Info "Configuration:"
    Write-Info "  Base URL: $BaseUrl"
    if ([string]::IsNullOrEmpty($ApiKey)) {
        Write-Info "  API Key: (none - using OAuth)"
    }
    else {
        $maskedKey = if ($ApiKey.Length -gt 12) {
            "$($ApiKey.Substring(0, 8))...$($ApiKey.Substring($ApiKey.Length - 4))"
        }
        else {
            "$($ApiKey.Substring(0, [Math]::Min(4, $ApiKey.Length)))..."
        }
        Write-Info "  API Key: $maskedKey"
    }
    Write-Info \"  Model: opus\"
    Write-Info \"  Max Output Tokens: 200000\"
    Write-Host \"\"

    # Skip API testing - directly proceed to configuration

    # Create settings directory
    New-SettingsDirectory

    # Backup existing settings
    Backup-Settings

    # Create new settings
    if (New-Settings -BaseUrl $BaseUrl -ApiKey $ApiKey) {
        Write-Host ""

        # Also set environment variables for Windows
        Write-Info "Setting environment variables..."
        try {
            # Set user environment variables (persistent across sessions)
            [Environment]::SetEnvironmentVariable("ANTHROPIC_BASE_URL", $BaseUrl, [EnvironmentVariableTarget]::User)
            [Environment]::SetEnvironmentVariable("ANTHROPIC_AUTH_TOKEN", $ApiKey, [EnvironmentVariableTarget]::User)

            # Also set for current session
            $env:ANTHROPIC_BASE_URL = $BaseUrl
            $env:ANTHROPIC_AUTH_TOKEN = $ApiKey

            Write-Success "Environment variables set successfully"
        }
        catch {
            Write-Warning "Failed to set environment variables: $($_.Exception.Message)"
            Write-Info "You may need to set them manually:"
            Write-Info "  ANTHROPIC_BASE_URL=$BaseUrl"
            Write-Info "  ANTHROPIC_AUTH_TOKEN=$ApiKey"
        }

        Write-Host ""
        Write-Success "Claude Code has been configured successfully!"
        Write-Info "Configuration details:"
        Write-Info "  - Local API endpoint: $BaseUrl"
        Write-Info "  - Model: opus (Claude Opus 4.5)"
        Write-Info "  - Max output tokens: 200000 (supports ~10000-15000 lines of code)"
        Write-Info "  - Telemetry disabled for privacy"
        Write-Host ""
        Write-Info "To verify the setup, run:"
        Write-Info "  claude --version"
        Write-Host ""
        Write-Info "Configuration file location: $ClaudeSettingsFile"
        Write-Info "Environment variables have been set for ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN"

        if (Test-Path $ClaudeSettingsFile) {
            Write-Host ""
            Write-Info "Current settings:"
            Write-Host "----------------------------------------"
            $settings = Get-Content $ClaudeSettingsFile -Raw | ConvertFrom-Json
            $settings | ConvertTo-Json -Depth 10
            Write-Host "----------------------------------------"
        }
    }
    else {
        Write-Error "Failed to create Claude Code settings"
        exit 1
    }
}

# Run main function
Main
