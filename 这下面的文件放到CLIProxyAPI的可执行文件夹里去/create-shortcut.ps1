$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut("$env:APPDATA\Microsoft\Windows\Start Menu\Programs\CLIProxyAPI.lnk")
$Shortcut.TargetPath = "D:\CliProxyAPI\CLIProxyAPI-Silent.vbs"
$Shortcut.WorkingDirectory = "D:\CliProxyAPI"
$Shortcut.Description = "CLI Proxy API"
$Shortcut.Save()
Write-Host "Shortcut created successfully"
