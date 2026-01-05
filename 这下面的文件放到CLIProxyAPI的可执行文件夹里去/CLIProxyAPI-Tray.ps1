# CLIProxyAPI System Tray Launcher
# Launch the service with a tray icon

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$exePath = "D:\CliProxyAPI\cli-proxy-api.exe"
$workDir = "D:\CliProxyAPI"

# Start main process (hidden window)
$process = Start-Process -FilePath $exePath -WorkingDirectory $workDir -WindowStyle Hidden -PassThru

# Create tray icon
$trayIcon = New-Object System.Windows.Forms.NotifyIcon
$trayIcon.Text = "CLIProxyAPI (PID: $($process.Id))"
$trayIcon.Visible = $true

# Try to use exe icon, otherwise use default
try {
    $trayIcon.Icon = [System.Drawing.Icon]::ExtractAssociatedIcon($exePath)
} catch {
    $trayIcon.Icon = [System.Drawing.SystemIcons]::Application
}

# Context menu
$contextMenu = New-Object System.Windows.Forms.ContextMenuStrip

# Status item
$menuStatus = New-Object System.Windows.Forms.ToolStripMenuItem
$menuStatus.Text = "Running (PID: $($process.Id))"
$menuStatus.Enabled = $false
$contextMenu.Items.Add($menuStatus) | Out-Null

$contextMenu.Items.Add("-") | Out-Null

# Restart option
$menuRestart = New-Object System.Windows.Forms.ToolStripMenuItem
$menuRestart.Text = "Restart"
$menuRestart.Add_Click({
    $script:process | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
    $script:process = Start-Process -FilePath $exePath -WorkingDirectory $workDir -WindowStyle Hidden -PassThru
    $trayIcon.Text = "CLIProxyAPI (PID: $($script:process.Id))"
    $menuStatus.Text = "Running (PID: $($script:process.Id))"
    $trayIcon.ShowBalloonTip(2000, "CLIProxyAPI", "Service restarted", [System.Windows.Forms.ToolTipIcon]::Info)
})
$contextMenu.Items.Add($menuRestart) | Out-Null

# Exit option
$menuExit = New-Object System.Windows.Forms.ToolStripMenuItem
$menuExit.Text = "Exit"
$menuExit.Add_Click({
    $script:process | Stop-Process -Force -ErrorAction SilentlyContinue
    $trayIcon.Visible = $false
    $trayIcon.Dispose()
    [System.Windows.Forms.Application]::Exit()
})
$contextMenu.Items.Add($menuExit) | Out-Null

$trayIcon.ContextMenuStrip = $contextMenu

# Double click to open logs
$trayIcon.Add_DoubleClick({
    $logDir = "D:\CliProxyAPI\logs"
    if (Test-Path $logDir) {
        Start-Process explorer.exe $logDir
    }
})

# Show startup notification
$trayIcon.ShowBalloonTip(2000, "CLIProxyAPI", "Service started", [System.Windows.Forms.ToolTipIcon]::Info)

# Monitor process status
$timer = New-Object System.Windows.Forms.Timer
$timer.Interval = 5000
$timer.Add_Tick({
    if ($script:process.HasExited) {
        $menuStatus.Text = "Stopped"
        $trayIcon.Text = "CLIProxyAPI (Stopped)"
    }
})
$timer.Start()

# Keep running
[System.Windows.Forms.Application]::Run()
