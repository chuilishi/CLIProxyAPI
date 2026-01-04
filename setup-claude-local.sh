#!/bin/bash
# Claude Code Configuration Script for CLIProxyAPI (Linux/WSL)
# This script configures Claude Code to use your local CLIProxyAPI instance
# Run with: bash setup-claude-local.sh
# Or make it executable: chmod +x setup-claude-local.sh && ./setup-claude-local.sh

# Configuration - Customized for CLIProxyAPI
DEFAULT_BASE_URL="http://localhost:8317"
DEFAULT_API_KEY="sk-dummy"  # Placeholder for OAuth login (Antigravity)
CLAUDE_CONFIG_DIR="$HOME/.claude"
CLAUDE_SETTINGS_FILE="$CLAUDE_CONFIG_DIR/settings.json"

# Command line arguments
BASE_URL=""
API_KEY=""
SHOW_SETTINGS=false
SHOW_HELP=false

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Output functions
info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to show help
show_help() {
    cat << EOF
Claude Code Configuration Script for CLIProxyAPI (Linux/WSL)

Usage: bash setup-claude-local.sh [OPTIONS]

Options:
  --base-url <URL>  Set the CLIProxyAPI base URL (default: $DEFAULT_BASE_URL)
  --api-key <KEY>   Set the API key (optional, can be empty for OAuth login)
  --show            Show current settings and exit
  --help            Show this help message

Examples:
  ./setup-claude-local.sh
  ./setup-claude-local.sh --base-url http://localhost:8317 --api-key sk-your-key
  ./setup-claude-local.sh --show

Quick setup (uses defaults):
  ./setup-claude-local.sh
EOF
    exit 0
}

# Function to backup existing settings
backup_settings() {
    if [ -f "$CLAUDE_SETTINGS_FILE" ]; then
        local timestamp=$(date +"%Y%m%d_%H%M%S")
        local backup_file="${CLAUDE_SETTINGS_FILE}.backup.${timestamp}"
        cp "$CLAUDE_SETTINGS_FILE" "$backup_file"
        info "Backed up existing settings to: $backup_file"
    fi
}

# Function to create settings directory
create_settings_directory() {
    if [ ! -d "$CLAUDE_CONFIG_DIR" ]; then
        mkdir -p "$CLAUDE_CONFIG_DIR"
        info "Created Claude configuration directory: $CLAUDE_CONFIG_DIR"
    fi
}

# Function to validate API key format
validate_api_key() {
    local api_key="$1"

    if [[ "$api_key" =~ ^[A-Za-z0-9_-]+$ ]]; then
        return 0
    else
        error "Invalid API key format. API key should contain only alphanumeric characters, hyphens, and underscores."
        return 1
    fi
}

# Function to test API connection
test_api_connection() {
    local base_url="$1"
    local api_key="$2"

    info "Testing API connection..."

    # Determine if it's a team URL
    if [[ "$base_url" == */team ]]; then
        local uri="${base_url}/api/v1/team/stats/spending"
        local balance_field="daily_remaining"
        local balance_label="Daily remaining"
    else
        local uri="${base_url}/api/v1/user/balance"
        local balance_field="balance"
        local balance_label="Current balance"
    fi

    # Make API call
    local response=$(curl -s -w "\n%{http_code}" -H "Content-Type: application/json" -H "X-API-Key: $api_key" "$uri" 2>&1)
    local http_code=$(echo "$response" | tail -n1)
    local body=$(echo "$response" | sed '$d')

    if [ "$http_code" = "200" ]; then
        # Try to extract balance using grep/sed (fallback if jq not available)
        if command -v jq &> /dev/null; then
            local balance=$(echo "$body" | jq -r ".$balance_field // empty")
        else
            local balance=$(echo "$body" | grep -o "\"$balance_field\":[0-9.]*" | cut -d: -f2)
        fi

        if [ -n "$balance" ]; then
            success "API connection successful! ${balance_label}: \$$balance"
            return 0
        else
            error "API test failed: Invalid response"
            return 1
        fi
    elif [ "$http_code" = "401" ]; then
        error "API key authentication failed. Please check your API key."
        return 1
    else
        error "Cannot connect to API server. Please check the URL and your internet connection."
        warning "Make sure your local API router is running on $base_url"
        return 1
    fi
}

# Function to create Claude Code settings
create_settings() {
    local base_url="$1"
    local api_key="$2"

    cat > "$CLAUDE_SETTINGS_FILE" << EOF
{
  "env": {
    "ANTHROPIC_BASE_URL": "$base_url",
    "ANTHROPIC_AUTH_TOKEN": "$api_key",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": 200000,
    "DISABLE_TELEMETRY": 1,
    "DISABLE_ERROR_REPORTING": 1,
    "CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR": 1,
    "MAX_THINKING_TOKENS": 10000,
    "CLAUDE_CODE_DISABLE_TERMINAL_TITLE": 1,
    "DISABLE_NON_ESSENTIAL_MODEL_CALLS": 1,
    "DISABLE_COST_WARNINGS": 1
  },
  "model": "opus"
}
EOF

    if [ $? -eq 0 ]; then
        success "Claude Code settings written to: $CLAUDE_SETTINGS_FILE"
        return 0
    else
        error "Failed to create settings file"
        return 1
    fi
}

# Function to display current settings
show_settings() {
    if [ -f "$CLAUDE_SETTINGS_FILE" ]; then
        info "Current Claude Code settings:"
        echo "----------------------------------------"
        cat "$CLAUDE_SETTINGS_FILE"
        echo "----------------------------------------"
    else
        info "No existing Claude Code settings found."
    fi

    echo ""
    info "Current environment variables:"
    echo "----------------------------------------"

    if [ -n "$ANTHROPIC_BASE_URL" ]; then
        info "ANTHROPIC_BASE_URL: $ANTHROPIC_BASE_URL"
    else
        info "ANTHROPIC_BASE_URL: (not set)"
    fi

    if [ -n "$ANTHROPIC_AUTH_TOKEN" ]; then
        local masked_token
        if [ ${#ANTHROPIC_AUTH_TOKEN} -gt 12 ]; then
            masked_token="${ANTHROPIC_AUTH_TOKEN:0:8}...${ANTHROPIC_AUTH_TOKEN: -4}"
        else
            masked_token="${ANTHROPIC_AUTH_TOKEN:0:4}..."
        fi
        info "ANTHROPIC_AUTH_TOKEN: $masked_token"
    else
        info "ANTHROPIC_AUTH_TOKEN: (not set)"
    fi
    echo "----------------------------------------"
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --base-url)
            BASE_URL="$2"
            shift 2
            ;;
        --api-key)
            API_KEY="$2"
            shift 2
            ;;
        --show)
            SHOW_SETTINGS=true
            shift
            ;;
        --help)
            SHOW_HELP=true
            shift
            ;;
        *)
            error "Unknown option: $1"
            show_help
            ;;
    esac
done

# Main function
main() {
    info "Claude Code Local Configuration Script"
    echo "======================================================="
    echo ""

    # Handle command line arguments
    if [ "$SHOW_HELP" = true ]; then
        show_help
    fi

    if [ "$SHOW_SETTINGS" = true ]; then
        show_settings
        exit 0
    fi

    # Use defaults if not provided
    if [ -z "$BASE_URL" ]; then
        BASE_URL="$DEFAULT_BASE_URL"
        info "Using default Base URL: $BASE_URL"
    fi

    if [ -z "$API_KEY" ]; then
        API_KEY="$DEFAULT_API_KEY"
        info "Using default API Key"
    fi

    # Validate API key
    if ! validate_api_key "$API_KEY"; then
        exit 1
    fi

    # Remove trailing slash from URL
    BASE_URL="${BASE_URL%/}"

    echo ""
    info "Configuration:"
    info "  Base URL: $BASE_URL"

    local masked_key
    if [ ${#API_KEY} -gt 12 ]; then
        masked_key="${API_KEY:0:8}...${API_KEY: -4}"
    else
        masked_key="${API_KEY:0:4}..."
    fi
    info "  API Key: $masked_key"
    info "  Model: opus"
    info "  Max Output Tokens: 200000"
    echo ""

    # Skip API testing - directly proceed to configuration

    # Create settings directory
    create_settings_directory

    # Backup existing settings
    backup_settings

    # Create new settings
    if create_settings "$BASE_URL" "$API_KEY"; then
        echo ""

        # Set environment variables for current shell
        info "Setting environment variables for current session..."
        export ANTHROPIC_BASE_URL="$BASE_URL"
        export ANTHROPIC_AUTH_TOKEN="$API_KEY"

        # Add to shell profile for persistence
        local shell_profile=""
        if [ -n "$BASH_VERSION" ]; then
            if [ -f "$HOME/.bashrc" ]; then
                shell_profile="$HOME/.bashrc"
            fi
        elif [ -n "$ZSH_VERSION" ]; then
            if [ -f "$HOME/.zshrc" ]; then
                shell_profile="$HOME/.zshrc"
            fi
        fi

        if [ -n "$shell_profile" ]; then
            # Check if variables already exist in profile
            if ! grep -q "ANTHROPIC_BASE_URL" "$shell_profile" 2>/dev/null; then
                echo "" >> "$shell_profile"
                echo "# Claude Code Local Configuration" >> "$shell_profile"
                echo "export ANTHROPIC_BASE_URL=\"$BASE_URL\"" >> "$shell_profile"
                echo "export ANTHROPIC_AUTH_TOKEN=\"$API_KEY\"" >> "$shell_profile"
                success "Environment variables added to $shell_profile"
                warning "Please run 'source $shell_profile' or restart your terminal to apply the changes"
            else
                warning "Environment variables already exist in $shell_profile"
                info "You may need to update them manually if the values have changed"
            fi
        else
            warning "Could not detect shell profile file"
            info "Please add these to your shell profile manually:"
            info "  export ANTHROPIC_BASE_URL=\"$BASE_URL\""
            info "  export ANTHROPIC_AUTH_TOKEN=\"$API_KEY\""
        fi

        echo ""
        success "Claude Code has been configured successfully!"
        info "Configuration details:"
        info "  - Local API endpoint: $BASE_URL"
        info "  - Model: opus (Claude Opus 4.5)"
        info "  - Max output tokens: 200000 (supports ~10000-15000 lines of code)"
        info "  - Telemetry disabled for privacy"
        echo ""
        info "To verify the setup, run:"
        info "  claude --version"
        echo ""
        info "Configuration file location: $CLAUDE_SETTINGS_FILE"

        if [ -f "$CLAUDE_SETTINGS_FILE" ]; then
            echo ""
            info "Current settings:"
            echo "----------------------------------------"
            cat "$CLAUDE_SETTINGS_FILE"
            echo "----------------------------------------"
        fi
    else
        error "Failed to create Claude Code settings"
        exit 1
    fi
}

# Run main function
main
