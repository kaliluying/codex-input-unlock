[CmdletBinding()]
param(
  [string]$BundlePath,
  [switch]$SkipPackageCheck
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$controller = Join-Path $repo 'codex-input-unlock.ps1'
$bin = Join-Path $repo 'codex-input-unlock.exe'
if (-not (Test-Path -LiteralPath $bin)) {
  $bin = Join-Path $repo 'codex-input-unlock'
}

foreach ($script in @($controller, $PSCommandPath)) {
  $tokens = $null
  $errors = $null
  [void][System.Management.Automation.Language.Parser]::ParseFile($script, [ref]$tokens, [ref]$errors)
  if ($errors.Count -gt 0) { throw "PowerShell parse failed for $script`: $($errors[0].Message)" }
}

if (-not $SkipPackageCheck) {
  $configPath = Join-Path $env:USERPROFILE '.codex\config.toml'
  $configHashBefore = if (Test-Path -LiteralPath $configPath -PathType Leaf) { (Get-FileHash -LiteralPath $configPath -Algorithm SHA256).Hash } else { $null }
  $diagnosisText = & pwsh -NoProfile -ExecutionPolicy Bypass -File $controller -Action Diagnose | Out-String
  if ($LASTEXITCODE -ne 0) { throw 'Diagnose failed' }
  $configHashAfter = if (Test-Path -LiteralPath $configPath -PathType Leaf) { (Get-FileHash -LiteralPath $configPath -Algorithm SHA256).Hash } else { $null }
  if ($configHashBefore -ne $configHashAfter) { throw 'Diagnose modified config.toml' }
}

$controllerText = Get-Content -LiteralPath $controller -Raw -Encoding UTF8
$forbiddenConfigMutationMarkers = @(
  'Enable-ProviderOpenAIAuth',
  'Restore-ConfigIfUnchanged',
  'configBackupPath',
  'patchedConfigHash',
  'configRestoreEligible',
  'Write-Utf8NoBom $Provider.ConfigPath',
  '[System.IO.File]::Copy($Provider.ConfigPath'
)
foreach ($marker in $forbiddenConfigMutationMarkers) {
  if ($controllerText.Contains($marker, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Controller still contains a forbidden config mutation path: $marker"
  }
}
if ($controllerText -notmatch 'configManagedByPatch\s*=\s*\$false') {
  throw 'Controller does not declare config.toml as user-managed'
}
if ($controllerText -notmatch 'function Test-StablePwshPath') {
  throw 'Controller does not distinguish version-pinned Store pwsh paths from stable ones'
}
if ($controllerText -match "Source\s+-notlike\s+'\*\\WindowsApps") {
  throw 'Controller rejects every Store pwsh path instead of only the version-pinned ones'
}
if ($controllerText -notmatch 'function Get-RunningOpenAIDesktopProcesses') {
  throw 'Controller does not scan all supported Codex and ChatGPT Desktop packages before launch'
}
if ($controllerText -notmatch '-Action Launch -PauseOnError') {
  throw 'Shortcut launch does not preserve the error window'
}
if ($controllerText -notmatch 'Launch blocked because Codex or ChatGPT Desktop is still running') {
  throw 'Controller does not provide the expected English running-app error'
}
if ($controllerText -notmatch 'Press Enter to close this window') {
  throw 'Controller does not provide an English close-window prompt'
}

if ($BundlePath) {
  if (-not (Test-Path -LiteralPath $bin)) {
    throw "Binary not found at $bin for bundle self-test"
  }
  $selfTestText = & $bin --self-test --bundle $BundlePath
  if ($LASTEXITCODE -ne 0) { throw 'Quota gate current-bundle self-test failed' }
  $selfTest = $selfTestText | ConvertFrom-Json
  if ($selfTest.location.mode -notin @('codex-local-quota', 'local-submit-quota', 'composer-quota-consumer', 'quota-atom-data', 'codex-rate-limit-derived-v2', 'codex-rate-limit-derived', 'codex-rate-limit-state')) { throw "Current bundle did not use a supported Codex local quota locator: $($selfTest.location.mode)" }
  if (-not $selfTest.location.quotaVariable) {
    throw 'Current bundle locator did not identify the local quota variable'
  }
}

Write-Host 'codex-input-unlock smoke tests passed'
