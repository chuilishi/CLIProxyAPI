Set WshShell = CreateObject("WScript.Shell")
WshShell.Run "powershell -ExecutionPolicy Bypass -WindowStyle Hidden -File ""D:\CliProxyAPI\CLIProxyAPI-Tray.ps1""", 0, False
