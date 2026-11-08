# ==============================================================================
# Codex / ChatGPT 输入框解锁 —— Windows PowerShell 控制器与环境调度器
#
# 支持动作 (-Action):
#   - Diagnose : 检查当前系统环境、Codex/ChatGPT 客户端安装状态以及二进制就绪情况
#   - Install  : 安装到 LocalAppData，生成桌面快捷方式并准备二进制运行环境
#   - Launch   : 一键拉起 Codex/ChatGPT 并开启 CDP 断点注入守护
#   - Verify   : 验证当前正在运行的解锁会话健康状态
#   - Inspect  : 查看当前的安装状态与配置详情
#   - Uninstall: 清理安装目录与桌面快捷方式
#   - Update   : 检查并在线升级到最新发布的版本
# ==============================================================================

[CmdletBinding()]
param(
  [ValidateSet('Diagnose', 'Install', 'Launch', 'Verify', 'Inspect', 'Uninstall', 'Update')]
  [string]$Action = 'Diagnose',
  [string]$InstallRoot = (Join-Path $env:LOCALAPPDATA 'OpenAI\CodexInputUnlock'),
  [int]$DebugPort = 0,
  [ValidateRange(15, 180)]
  [int]$StartupTimeoutSeconds = 60,
  [switch]$DisableComputerUseBootstrap,
  [switch]$NoShortcut,
  [switch]$PauseOnError,
  [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:SourceRoot = Split-Path -Parent $PSCommandPath
$script:StateDirectory = Join-Path $InstallRoot 'state'
$script:LogDirectory = Join-Path $InstallRoot 'logs'
$script:StatePath = Join-Path $script:StateDirectory 'install-state.json'
$script:SettingsPath = Join-Path $script:StateDirectory 'settings.json'
$script:RuntimeController = Join-Path $InstallRoot 'codex-input-unlock.ps1'
$script:RuntimeSupervisor = Join-Path $InstallRoot 'codex-input-unlock.exe'

function Write-Info([string]$Message) {
  Write-Host "[codex-input-unlock] $Message"
}

function Write-Utf8NoBom([string]$Path, [string]$Content) {
  $parent = Split-Path -Parent $Path
  if ($parent) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
  [System.IO.File]::WriteAllText($Path, $Content, [System.Text.UTF8Encoding]::new($false))
}

function Get-OptionalProperty([object]$InputObject, [string]$Name) {
  if ($null -eq $InputObject) { return $null }
  if ($InputObject.PSObject.Properties.Name -notcontains $Name) { return $null }
  return $InputObject.$Name
}

function Read-JsonFile([string]$Path) {
  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
  return Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json
}

function Write-JsonFile([string]$Path, [object]$Value) {
  Write-Utf8NoBom $Path (($Value | ConvertTo-Json -Depth 10) + "`n")
}

function Resolve-CodexPackage {
  $specs = @(
    [pscustomobject]@{ Name = 'OpenAI.Codex'; Priority = 3 },
    [pscustomobject]@{ Name = 'OpenAI.CodexBeta'; Priority = 2 },
    [pscustomobject]@{ Name = 'OpenAI.ChatGPT-Desktop'; Priority = 1 }
  )
  $candidates = foreach ($spec in $specs) {
    foreach ($package in @(Get-AppxPackage -Name $spec.Name -ErrorAction SilentlyContinue)) {
      [pscustomobject]@{ Package = $package; Priority = $spec.Priority }
    }
  }
  $selected = $candidates | Sort-Object Priority, @{ Expression = { [version]$_.Package.Version } } -Descending | Select-Object -First 1
  if (-not $selected) { throw 'No supported OpenAI Codex/ChatGPT Desktop Store package is installed.' }

  $manifestPath = Join-Path $selected.Package.InstallLocation 'AppxManifest.xml'
  [xml]$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8
  $applications = @($manifest.Package.Applications.Application)
  $application = $applications | Where-Object { $_.Id -eq 'App' } | Select-Object -First 1
  if (-not $application) { $application = $applications | Select-Object -First 1 }
  if (-not $application -or -not $application.Id) { throw "No application entry found in $manifestPath" }
  $executable = Join-Path $selected.Package.InstallLocation ([string]$application.Executable)
  [pscustomobject]@{
    Name = $selected.Package.Name
    Version = $selected.Package.Version.ToString()
    PackageFullName = $selected.Package.PackageFullName
    PackageFamilyName = $selected.Package.PackageFamilyName
    InstallLocation = $selected.Package.InstallLocation
    AppId = [string]$application.Id
    Aumid = "$($selected.Package.PackageFamilyName)!$($application.Id)"
    ManifestPath = $manifestPath
  }
}

function Get-CodexPackageProcesses([object]$Package) {
  $prefix = [System.IO.Path]::GetFullPath($Package.InstallLocation).TrimEnd('\') + '\'
  @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
    $_.ExecutablePath -and [System.IO.Path]::GetFullPath($_.ExecutablePath).StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)
  } | Select-Object ProcessId, Name, ExecutablePath, CommandLine)
}

function Get-RunningOpenAIDesktopProcesses {
  $packageNames = @('OpenAI.Codex', 'OpenAI.CodexBeta', 'OpenAI.ChatGPT-Desktop')
  $packages = @($packageNames | ForEach-Object {
    Get-AppxPackage -Name $_ -ErrorAction SilentlyContinue
  })
  if ($packages.Count -eq 0) { return @() }

  $packageRoots = @($packages | ForEach-Object {
    [pscustomobject]@{
      PackageName = $_.Name
      Prefix = [System.IO.Path]::GetFullPath($_.InstallLocation).TrimEnd('\') + '\'
    }
  })
  $matches = foreach ($process in @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue)) {
    if (-not $process.ExecutablePath) { continue }
    $executablePath = [System.IO.Path]::GetFullPath($process.ExecutablePath)
    $packageRoot = $packageRoots | Where-Object {
      $executablePath.StartsWith($_.Prefix, [System.StringComparison]::OrdinalIgnoreCase)
    } | Select-Object -First 1
    if ($packageRoot) {
      [pscustomobject]@{
        ProcessId = $process.ProcessId
        Name = $process.Name
        PackageName = $packageRoot.PackageName
        ExecutablePath = $process.ExecutablePath
        CommandLine = $process.CommandLine
      }
    }
  }
  @($matches | Sort-Object ProcessId -Unique)
}

function Invoke-WithPatchMutex([scriptblock]$Operation) {
  $normalized = [System.IO.Path]::GetFullPath($InstallRoot).ToLowerInvariant()
  $bytes = [System.Text.Encoding]::UTF8.GetBytes($normalized)
  $hash = [System.Security.Cryptography.SHA256]::HashData($bytes)
  $suffix = ([Convert]::ToHexString($hash)).Substring(0, 20)
  $mutex = [System.Threading.Mutex]::new($false, "Local\CodexInputUnlock-$suffix")
  $acquired = $false
  try {
    $acquired = $mutex.WaitOne(0)
    if (-not $acquired) { throw 'Another Codex Input Unlock operation (install, launch, or uninstall) is already running.' }
    & $Operation
  } finally {
    if ($acquired) { [void]$mutex.ReleaseMutex() }
    $mutex.Dispose()
  }
}

function Ensure-ActivationType {
  if ('CodexInputUnlock.ApplicationActivator' -as [type]) { return }
  Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

namespace CodexInputUnlock {
  [Flags]
  public enum ActivateOptions : uint {
    None = 0
  }

  [ComImport]
  [Guid("2E941141-7F97-4756-BA1D-9DECDE894A3D")]
  [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
  interface IApplicationActivationManager {
    int ActivateApplication(
      [MarshalAs(UnmanagedType.LPWStr)] string appUserModelId,
      [MarshalAs(UnmanagedType.LPWStr)] string arguments,
      ActivateOptions options,
      out uint processId);
    int ActivateForFile(string appUserModelId, IntPtr itemArray, string verb, out uint processId);
    int ActivateForProtocol(string appUserModelId, IntPtr itemArray, out uint processId);
  }

  [ComImport]
  [Guid("45BA127D-10A8-46EA-8AB7-56EA9078943C")]
  class ApplicationActivationManager {}

  public static class ApplicationActivator {
    public static uint Activate(string appUserModelId, string arguments) {
      var manager = (IApplicationActivationManager)new ApplicationActivationManager();
      uint processId;
      int result = manager.ActivateApplication(appUserModelId, arguments, ActivateOptions.None, out processId);
      Marshal.ThrowExceptionForHR(result);
      return processId;
    }
  }
}
'@
}

function Get-FreeTcpPort {
  $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
  $listener.Start()
  try { return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port } finally { $listener.Stop() }
}

function Quote-WindowsArgument([string]$Value) {
  if ($Value -notmatch '[\s"]') { return $Value }
  return '"' + ([regex]::Replace($Value, '(\\*)"', '$1$1\"') -replace '(\\+)$', '$1$1') + '"'
}

function Test-StablePwshPath {
  param([string]$Path)
  # WindowsApps 下的包目录名形如 Microsoft.PowerShell_7.6.7.0_x64__8wekyb3d8bbwe，带版本号的那一段会随
  # Store 包升级改名，写进快捷方式等于埋雷；包家族名目录 (Microsoft.PowerShell_8wekyb3d8bbwe) 与应用执行
  # 别名 (WindowsApps\pwsh.exe) 都不含版本号，可以安全写进快捷方式。
  if ([string]::IsNullOrEmpty($Path)) { return $false }
  return $Path -notmatch '\\WindowsApps\\[^\\]+_\d+(?:\.\d+)+_[^\\]*'
}

function Resolve-PwshExecutable {
  # 顺序即优先级：MSI 固定位置 → Store 包家族名目录 → Store 应用执行别名 → PATH 上的 pwsh（仍须通过稳定性检查）。
  $candidates = @(
    (Join-Path $env:ProgramFiles 'PowerShell\7\pwsh.exe')
    (Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps\Microsoft.PowerShell_8wekyb3d8bbwe\pwsh.exe')
    (Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps\pwsh.exe')
  )
  $command = Get-Command pwsh -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($command -and $command.Source) { $candidates += $command.Source }
  foreach ($candidate in $candidates) {
    if ((Test-StablePwshPath $candidate) -and (Test-Path -LiteralPath $candidate -PathType Leaf)) { return $candidate }
  }
  throw 'No stable PowerShell 7 found. Install PowerShell 7 from the MSI package or the Microsoft Store, then re-run Install.'
}

function New-Shortcut([string]$ControllerPath) {
  $startMenu = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs'
  $shortcutPath = Join-Path $startMenu 'Codex Input Unlock.lnk'
  $shell = New-Object -ComObject WScript.Shell
  $shortcut = $shell.CreateShortcut($shortcutPath)
  $shortcut.TargetPath = Resolve-PwshExecutable
  $shortcut.Arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$ControllerPath`" -Action Launch -PauseOnError"
  $shortcut.WorkingDirectory = Split-Path -Parent $ControllerPath
  $shortcut.Description = 'Launch official Codex with input unlocked for third-party providers'
  $shortcut.Save()
  return $shortcutPath
}

function Invoke-Diagnose {
  $package = Resolve-CodexPackage
  $pwsh = try { Resolve-PwshExecutable } catch { $null }
  $activatorReady = $false
  try { Ensure-ActivationType; $activatorReady = $true } catch { $activatorReady = $false }
  $state = Read-JsonFile $script:StatePath
  $requiredRuntime = @($script:RuntimeController, $script:RuntimeSupervisor, $script:SettingsPath, $script:StatePath)
  $installed = @($requiredRuntime | Where-Object { -not (Test-Path -LiteralPath $_ -PathType Leaf) }).Count -eq 0
  [pscustomobject]@{
    Package = $package
    RunningPackageProcesses = @(Get-CodexPackageProcesses $package)
    Pwsh = if ($pwsh) { $pwsh } else { $null }
    PwshUsable = $null -ne $pwsh
    ActivatorReady = $activatorReady
    Installed = $installed
    MissingRuntimeFiles = @($requiredRuntime | Where-Object { -not (Test-Path -LiteralPath $_ -PathType Leaf) })
    InstallRoot = $InstallRoot
    State = $state
    UserComputerUseEnvironment = [Environment]::GetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', 'User')
  }
}

function Ensure-Executable {
  $localBin = Join-Path $script:SourceRoot 'codex-input-unlock.exe'
  if (Test-Path -LiteralPath $script:RuntimeSupervisor -PathType Leaf) {
    return $script:RuntimeSupervisor
  }
  if (Test-Path -LiteralPath $localBin -PathType Leaf) {
    return $localBin
  }

  $go = Get-Command go -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($go) {
    Write-Info 'Compiling codex-input-unlock.exe from source...'
    $prevLocation = Get-Location
    try {
      Set-Location $script:SourceRoot
      & $go.Source build -trimpath -ldflags="-s -w" -o $localBin .
      if ($LASTEXITCODE -eq 0 -and (Test-Path -LiteralPath $localBin -PathType Leaf)) {
        return $localBin
      }
    } finally {
      Set-Location $prevLocation
    }
  }

  Write-Info 'Executable not found and Go compiler not installed. Downloading prebuilt binary from GitHub Release...'
  $releaseRepo = if ($env:CODEX_UNLOCK_RELEASE_REPO) { $env:CODEX_UNLOCK_RELEASE_REPO } else { 'kaliluying/codex-input-unlock' }
  $arch = if ([System.Environment]::Is64BitOperatingSystem) {
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
  } else {
    '386'
  }
  $assetName = "codex-input-unlock-windows-$arch.exe"
  $url = "https://github.com/$releaseRepo/releases/latest/download/$assetName"
  Write-Info "Downloading $url -> $localBin..."
  try {
    Invoke-WebRequest -Uri $url -OutFile $localBin -UseBasicParsing
  } catch {
    throw "Failed to download $assetName from $url. Error: $($_.Exception.Message). Please download it manually and place it at $localBin"
  }
  if (-not (Test-Path -LiteralPath $localBin -PathType Leaf)) {
    throw "Downloaded binary was not found at $localBin"
  }
  return $localBin
}

function Invoke-Install {
  $package = Resolve-CodexPackage
  Ensure-ActivationType
  if ($DryRun) {
    return [pscustomobject]@{ DryRun = $true; Package = $package; InstallRoot = $InstallRoot; ActivatorReady = $true }
  }
  Ensure-Executable | Out-Null
  $requiredFiles = @('codex-input-unlock.ps1', 'codex-input-unlock.exe')
  $optionalFiles = @('README.md', 'ATTRIBUTION.md', 'LICENSE', 'SECURITY.md')
  foreach ($file in $requiredFiles) {
    $source = Join-Path $script:SourceRoot $file
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) { throw "Required runtime file missing: $source" }
  }
  $previousState = Read-JsonFile $script:StatePath
  $hasOriginalEnvironment = $previousState -and $previousState.PSObject.Properties.Name -contains 'originalComputerUseEnvironment'
  $originalEnv = if ($hasOriginalEnvironment) {
    $previousState.originalComputerUseEnvironment
  } else {
    [Environment]::GetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', 'User')
  }
  $currentEnv = [Environment]::GetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', 'User')
  $changedEnvironmentThisRun = $false
  $shortcut = $null
  try {
    New-Item -ItemType Directory -Path $InstallRoot -Force | Out-Null
    New-Item -ItemType Directory -Path $script:StateDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $script:LogDirectory -Force | Out-Null
    foreach ($file in @($requiredFiles + $optionalFiles)) {
      $source = Join-Path $script:SourceRoot $file
      if (-not (Test-Path -LiteralPath $source -PathType Leaf)) { continue }
      $destination = Join-Path $InstallRoot $file
      if ([System.IO.Path]::GetFullPath($source) -ne [System.IO.Path]::GetFullPath($destination)) {
        Copy-Item -LiteralPath $source -Destination $destination -Force
      }
    }
    if (-not $DisableComputerUseBootstrap -and $currentEnv -ne '1') {
      [Environment]::SetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', '1', 'User')
      $changedEnvironmentThisRun = $true
    }
    $settings = [pscustomobject]@{
      enableComputerUseBootstrap = -not $DisableComputerUseBootstrap
      startupTimeoutSeconds = $StartupTimeoutSeconds
    }
    Write-JsonFile $script:SettingsPath $settings
    $shortcut = if ($NoShortcut) { $null } else { New-Shortcut $script:RuntimeController }
    $state = [ordered]@{
      schemaVersion = 4
      installedAt = (Get-Date).ToString('o')
      installRoot = $InstallRoot
      configManagedByPatch = $false
      originalComputerUseEnvironment = $originalEnv
      computerUseEnvironmentChanged = $originalEnv -ne '1'
      shortcutPath = $shortcut
      supervisorPid = Get-OptionalProperty $previousState 'supervisorPid'
      appPid = Get-OptionalProperty $previousState 'appPid'
      debugPort = Get-OptionalProperty $previousState 'debugPort'
      lastStatusPath = Get-OptionalProperty $previousState 'lastStatusPath'
      lastLaunchAt = Get-OptionalProperty $previousState 'lastLaunchAt'
    }
    Write-JsonFile $script:StatePath $state
  } catch {
    if ($changedEnvironmentThisRun) { [Environment]::SetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', $currentEnv, 'User') }
    if ($shortcut -and (Test-Path -LiteralPath $shortcut)) { Remove-Item -LiteralPath $shortcut -Force -ErrorAction SilentlyContinue }
    throw
  }
  return [pscustomobject]@{ Installed = $true; InstallRoot = $InstallRoot; Shortcut = $shortcut; Package = $package.PackageFullName }
}

function Invoke-Launch {
  if (-not (Test-Path -LiteralPath $script:RuntimeSupervisor -PathType Leaf)) {
    throw "Codex Input Unlock is not installed at $InstallRoot. Run -Action Install first."
  }
  $settings = Read-JsonFile $script:SettingsPath
  $package = Resolve-CodexPackage
  $running = @(Get-RunningOpenAIDesktopProcesses)
  if ($running.Count -gt 0) {
    $processSummary = ($running | ForEach-Object { "$($_.PackageName)/$($_.Name) (PID $($_.ProcessId))" }) -join ', '
    throw "Launch blocked because Codex or ChatGPT Desktop is still running: $processSummary. Quit the application normally, including its system tray icon, and then launch Codex Input Unlock again. Codex Input Unlock did not close any running application."
  }
  $port = if ($DebugPort -gt 0) { $DebugPort } else { Get-FreeTcpPort }
  if ($port -lt 1 -or $port -gt 65535) { throw "Invalid debug port: $port" }

  $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
  $statusPath = Join-Path $script:StateDirectory "runtime-status-$stamp.json"
  $stdoutPath = Join-Path $script:LogDirectory "supervisor-$stamp.stdout.log"
  $stderrPath = Join-Path $script:LogDirectory "supervisor-$stamp.stderr.log"
  $arguments = @(
    'supervise',
    '--port', [string]$port,
    '--status', $statusPath,
    '--startup-timeout', [string]$StartupTimeoutSeconds
  )
  $supervisorArgumentString = ($arguments | ForEach-Object { Quote-WindowsArgument ([string]$_) }) -join ' '
  $supervisor = Start-Process -FilePath $script:RuntimeSupervisor -ArgumentList $supervisorArgumentString -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath
  try {
    if (-not $DisableComputerUseBootstrap -and (!$settings -or $settings.enableComputerUseBootstrap -ne $false)) {
      [Environment]::SetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', '1', 'Process')
    }
    Ensure-ActivationType
    $launchArguments = @(
      "--remote-debugging-address=127.0.0.1",
      "--remote-debugging-port=$port",
      "--remote-allow-origins=http://127.0.0.1:$port"
    )
    $argumentString = ($launchArguments | ForEach-Object { Quote-WindowsArgument $_ }) -join ' '
    $appPid = [CodexInputUnlock.ApplicationActivator]::Activate($package.Aumid, $argumentString)

    $deadline = (Get-Date).AddSeconds($StartupTimeoutSeconds)
    $runtime = $null
    while ((Get-Date) -lt $deadline) {
      if (Test-Path -LiteralPath $statusPath -PathType Leaf) {
        try { $runtime = Read-JsonFile $statusPath } catch { $runtime = $null }
        if ($runtime -and $runtime.state -eq 'ready') { break }
        if ($runtime -and $runtime.state -eq 'failed') { throw "Supervisor failed: $($runtime.error)" }
      }
      if ($supervisor.HasExited) { throw "Supervisor exited with code $($supervisor.ExitCode). See $stderrPath" }
      Start-Sleep -Milliseconds 250
    }
    if (-not $runtime -or $runtime.state -ne 'ready') {
      $detail = $null
      if (Test-Path -LiteralPath $statusPath -PathType Leaf) {
        try { $final = Read-JsonFile $statusPath } catch { $final = $null }
        if ($final) {
          if ($final.state -eq 'failed' -and $final.error) { throw "Supervisor failed: $($final.error)" }
          if ($final.lastError) { $detail = " Last state: $($final.state). Last error: $($final.lastError)" }
          elseif ($final.state) { $detail = " Last state: $($final.state)." }
        }
      }
      throw "Codex Input Unlock did not become ready within $StartupTimeoutSeconds seconds.$detail"
    }

    $state = Read-JsonFile $script:StatePath
    if (-not $state) { $state = [pscustomobject]@{} }
    $state | Add-Member -NotePropertyName configManagedByPatch -NotePropertyValue $false -Force
    $state | Add-Member -NotePropertyName supervisorPid -NotePropertyValue $supervisor.Id -Force
    $state | Add-Member -NotePropertyName appPid -NotePropertyValue $appPid -Force
    $state | Add-Member -NotePropertyName debugPort -NotePropertyValue $port -Force
    $state | Add-Member -NotePropertyName lastStatusPath -NotePropertyValue $statusPath -Force
    $state | Add-Member -NotePropertyName lastLaunchAt -NotePropertyValue (Get-Date).ToString('o') -Force
    Write-JsonFile $script:StatePath $state
    return [pscustomobject]@{ Ready = $true; AppPid = $appPid; SupervisorPid = $supervisor.Id; DebugPort = $port; Status = $runtime; ConfigModified = $false }
  } catch {
    if (-not $supervisor.HasExited) { Stop-Process -Id $supervisor.Id -Force -ErrorAction SilentlyContinue }
    throw
  }
}

function Invoke-Verify {
  $diagnosis = Invoke-Diagnose
  $state = Read-JsonFile $script:StatePath
  $lastStatusPath = Get-OptionalProperty $state 'lastStatusPath'
  $supervisorPid = Get-OptionalProperty $state 'supervisorPid'
  $debugPort = Get-OptionalProperty $state 'debugPort'
  $runtime = if ($lastStatusPath) { Read-JsonFile ([string]$lastStatusPath) } else { $null }
  $supervisorAlive = $false
  $supervisorProcess = $null
  if ($supervisorPid) {
    $supervisorProcess = Get-CimInstance Win32_Process -Filter "ProcessId=$([int]$supervisorPid)" -ErrorAction SilentlyContinue
    $supervisorAlive = $null -ne $supervisorProcess -and $supervisorProcess.Name -eq 'codex-input-unlock.exe'
  }
  $cdpListening = $false
  $cdpResponding = $false
  if ($debugPort) {
    $cdpListening = @(Get-NetTCPConnection -State Listen -LocalPort ([int]$debugPort) -ErrorAction SilentlyContinue).Count -gt 0
    if ($cdpListening) {
      try {
        $targets = Invoke-RestMethod -Uri "http://127.0.0.1:$([int]$debugPort)/json" -TimeoutSec 2
        $cdpResponding = @($targets | Where-Object { $_.type -eq 'page' -and $_.url -like 'app://-*' }).Count -gt 0
      } catch { $cdpResponding = $false }
    }
  }
  $runtimeFresh = $false
  if ($runtime -and $runtime.updatedAt) {
    try {
      $updatedAt = if ($runtime.updatedAt -is [datetime]) {
        $runtime.updatedAt.ToUniversalTime()
      } else {
        [datetime]::Parse([string]$runtime.updatedAt).ToUniversalTime()
      }
      $runtimeFresh = ((Get-Date).ToUniversalTime() - $updatedAt).TotalSeconds -lt 10
    } catch { $runtimeFresh = $false }
  }
  $runtimeReady = $runtime -and $runtime.state -eq 'ready' -and $runtimeFresh -and $supervisorAlive -and $cdpListening -and $cdpResponding
  [pscustomobject]@{
    Installed = $diagnosis.Installed
    PackageFullName = $diagnosis.Package.PackageFullName
    Runtime = $runtime
    SupervisorAlive = $supervisorAlive
    SupervisorProcess = if ($supervisorProcess) { [pscustomobject]@{ ProcessId = $supervisorProcess.ProcessId; Name = $supervisorProcess.Name; CommandLineMatches = $supervisorAlive } } else { $null }
    CdpListening = $cdpListening
    CdpResponding = $cdpResponding
    RuntimeFresh = $runtimeFresh
    RuntimeReady = $runtimeReady
    ConfigManagedByPatch = $false
    Note = 'Runtime readiness requires a cold launch through the Codex Input Unlock shortcut.'
  }
}

function Invoke-Inspect {
  $state = Read-JsonFile $script:StatePath
  $lastStatusPath = Get-OptionalProperty $state 'lastStatusPath'
  $supervisorPid = Get-OptionalProperty $state 'supervisorPid'
  $debugPort = Get-OptionalProperty $state 'debugPort'
  $runtime = if ($lastStatusPath -and (Test-Path -LiteralPath ([string]$lastStatusPath))) { Read-JsonFile ([string]$lastStatusPath) } else { $null }
  $process = if ($supervisorPid) { Get-CimInstance Win32_Process -Filter "ProcessId=$([int]$supervisorPid)" -ErrorAction SilentlyContinue } else { $null }
  $listener = if ($debugPort) { @(Get-NetTCPConnection -State Listen -LocalPort ([int]$debugPort) -ErrorAction SilentlyContinue | Select-Object LocalAddress, LocalPort, OwningProcess) } else { @() }
  $logs = @(Get-ChildItem -LiteralPath $script:LogDirectory -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 4 | ForEach-Object {
    [pscustomobject]@{ Path = $_.FullName; LastWriteTime = $_.LastWriteTime; Length = $_.Length; Tail = @(Get-Content -LiteralPath $_.FullName -Encoding UTF8 -Tail 80) }
  })
  [pscustomobject]@{
    CapturedAt = (Get-Date).ToString('o')
    State = $state
    Runtime = $runtime
    SupervisorProcess = if ($process) { [pscustomobject]@{ ProcessId = $process.ProcessId; Name = $process.Name; ExecutablePath = $process.ExecutablePath; CommandLine = $process.CommandLine } } else { $null }
    Listener = $listener
    ConfigManagedByPatch = $false
    Logs = $logs
  } | ConvertTo-Json -Depth 20
}

function Invoke-Uninstall {
  $state = Read-JsonFile $script:StatePath
  if ($DryRun) {
    return [pscustomobject]@{ DryRun = $true; InstallRoot = $InstallRoot; State = $state }
  }
  if ($state -and $state.supervisorPid) {
    $process = Get-CimInstance Win32_Process -Filter "ProcessId=$([int]$state.supervisorPid)" -ErrorAction SilentlyContinue
    if ($process -and ($process.Name -eq 'codex-input-unlock.exe' -or $process.CommandLine -like "*cdp-supervisor.mjs*")) {
      Stop-Process -Id ([int]$state.supervisorPid) -Force -ErrorAction SilentlyContinue
    }
  }
  if ($state -and $state.computerUseEnvironmentChanged) {
    $currentEnv = [Environment]::GetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', 'User')
    if ($currentEnv -eq '1') {
      [Environment]::SetEnvironmentVariable('CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE', $state.originalComputerUseEnvironment, 'User')
    } else {
      Write-Warning 'Computer Use environment variable changed after installation; automatic restore was skipped.'
    }
  }
  if ($state -and $state.shortcutPath -and (Test-Path -LiteralPath $state.shortcutPath)) {
    Remove-Item -LiteralPath $state.shortcutPath -Force
  }
  if (Test-Path -LiteralPath $InstallRoot) {
    $resolved = [System.IO.Path]::GetFullPath($InstallRoot).TrimEnd('\')
    $expectedDefault = [System.IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'OpenAI\CodexInputUnlock')).TrimEnd('\')
    $expectedLegacyPatch = [System.IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'OpenAI\CodexExternalPatch')).TrimEnd('\')
    $expectedLegacyComposer = [System.IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'OpenAI\CodexComposerUnlock')).TrimEnd('\')
    $expectedState = if ($state -and $state.installRoot) { [System.IO.Path]::GetFullPath([string]$state.installRoot).TrimEnd('\') } else { $null }
    if ($resolved -ne $expectedDefault -and $resolved -ne $expectedLegacyPatch -and $resolved -ne $expectedLegacyComposer -and $resolved -ne $expectedState) {
      throw "Refusing to remove unexpected install root: $resolved"
    }
    Remove-Item -LiteralPath $resolved -Recurse -Force
  }
  [pscustomobject]@{ Uninstalled = $true; ConfigModified = $false; OfficialPackageUntouched = $true }
}

try {
  switch ($Action) {
    'Diagnose' { Invoke-Diagnose }
    'Install' { Invoke-WithPatchMutex { Invoke-Install } }
    'Launch' { Invoke-WithPatchMutex { Invoke-Launch } }
    'Verify' { Invoke-Verify }
    'Inspect' { Invoke-Inspect }
    'Uninstall' { Invoke-WithPatchMutex { Invoke-Uninstall } }
    'Update' {
      $bin = Ensure-SupervisorBinary
      & $bin update
    }
  }
} catch {
  Write-Host "[codex-input-unlock] ERROR: $($_.Exception.Message)" -ForegroundColor Red
  if ($PauseOnError) {
    [void](Read-Host 'Press Enter to close this window')
  }
  exit 1
}
