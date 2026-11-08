# Codex Input Unlock (`codex-input-unlock`)

English | [简体中文](README.md)

An **external runtime input unlocker** for the official Codex Desktop and ChatGPT Desktop applications on Windows and macOS.

> **Upstream & Acknowledgements**:
> This project is a standalone fork modified from the upstream project [1zero224/codex-input-unlock](https://github.com/1zero224/codex-input-unlock). Thanks to [1zero224](https://github.com/1zero224) for proposing the original non-pausing CDP conditional breakpoint unlock approach.
>
> Key enhancements in this standalone version:
> 1. **Rewritten in Pure Go**: Fully replaces the supervisor and regex engine with Go standard library, **completely eliminating the Node.js runtime dependency**.
> 2. **Single Standalone Binary**: Compiles to a small, self-contained executable for both macOS and Windows.
> 3. **Zero Configuration & Auto-download**: Automatically fetches the matching prebuilt release binary when no Go compiler is installed locally.

When a user has configured a **third-party API provider, relay, or self-hosted gateway**, the official desktop client may still disable the composer and send button if the signed-in official account has exhausted its quota. Codex Input Unlock uses Chrome DevTools Protocol (CDP) to install a dynamic, non-pausing conditional breakpoint that **removes this local frontend input and send gate**.

> [!IMPORTANT]
> **Scope and disclaimer:**
> This project does **not and cannot** bypass, restore, or increase an OpenAI account's official quota. Its only purpose is to remove an unnecessary local frontend gate when you are already using **your own third-party API key and provider**.

---

## Features

- 🛡️ **No package modification:** Does not modify the Store Appx package, ASAR archive, or executable files. The official package and its signature remain untouched.
- 🔒 **No configuration management:** Never modifies, overwrites, or synchronizes `~/.codex/config.toml` or `auth.json`. You remain responsible for your own configuration.
- ⚡ **Non-pausing conditional breakpoint:** Uses a JavaScript comma expression such as `(quota = false, false)` to update the local variable while the breakpoint condition always evaluates to false, so the page never pauses.
- 🔄 **Automatic recovery after reload:** Watches the renderer lifecycle and reinstalls the breakpoint after a page reload or `Ctrl + R`.

---

## How it works

```text
Launcher / Controller (codex-input-unlock / codex-input-unlock.ps1)
  ├─ Finds the official client application (macOS .app / Windows Store package)
  ├─ Allocates a random TCP port and starts the native Go CDP supervisor
  └─ Activates the client with remote-debugging arguments
                        │
                        ▼
Native Go Supervisor (codex-input-unlock)
  ├─ Connects to the client renderer through CDP (built-in RFC 6455 WebSocket engine)
  ├─ Locates the quota gate in the frontend JavaScript bundle (built-in 9 pattern locators)
  ├─ Installs a conditional breakpoint: (quota = false, false)
  │    └─ Clears the local gate without pausing the renderer
  └─ Reinstalls the injection after a page reload or Ctrl + R
```

---

## Requirements

- **Operating system:** Windows 10 / 11 (x64 / ARM64) or macOS (Apple Silicon / Intel)
- **Official client:** Codex Desktop or ChatGPT Desktop
- **Runtime dependencies:** Zero external runtime dependencies (no Node.js required; single standalone binary)

---

## Quick start

### Configure your provider first

Before launching Codex Input Unlock, configure and verify your third-party provider in `~/.codex/config.toml`. This project does not create, modify, or validate provider configuration for you.

If you want to retain your official Codex account login while using the third-party provider, set the following option in the corresponding provider configuration:

```toml
requires_openai_auth = true
```

You must also sign in to your Codex account in Codex Desktop before using the unlocker. The third-party provider, `requires_openai_auth = true`, and the Codex login state must already be configured. Codex Input Unlock only removes the local input and send gate caused by the official quota state; it does not replace provider setup or account login.

### Quick Start

> **Important:** Completely quit every running Codex Desktop or ChatGPT Desktop process before launching, including the system tray icon. Launching from the original official shortcut does not include the required debugging arguments and therefore does not load the unlocker.

#### macOS Users
- **Option A (Desktop Click)**: Double-click **`启动 Codex (Mac).command`** directly in Finder.
- **Option B (Command Line)**:
  ```bash
  # Launch and supervise automatically
  ./codex-input-unlock

  # Diagnose local environment
  ./codex-input-unlock diagnose
  ```

#### Windows Users
- **Option A (Start Menu Shortcut)**:
  Open PowerShell 7 in the repository root and run:
  ```powershell
  pwsh -NoProfile -ExecutionPolicy Bypass -File '.\codex-input-unlock.ps1' -Action Install
  ```
  Then click **`Codex Input Unlock`** from the Windows Start menu.
- **Option B (Command Line)**:
  ```powershell
  .\codex-input-unlock.exe
  ```

---

## Testing & Self-test

Run automated tests without launching the desktop client:

```bash
# Run all unit tests (includes mock bundle regressions and real local app ASAR probe)
go test -v ./...

# Offline breakpoint location self-test on an unpacked bundle
./codex-input-unlock --self-test --bundle <path-to-js-bundle>
```

---

## Commands

The installed entry script is located at:

`$env:LOCALAPPDATA\OpenAI\CodexInputUnlock\codex-input-unlock.ps1`

| Action | Description |
| :--- | :--- |
| `-Action Diagnose` | Checks runtime dependencies, the installed Store package, and required runtime files |
| `-Action Verify` | Checks the supervisor process, CDP connection, and breakpoint readiness |
| `-Action Inspect` | Prints detailed diagnostic JSON containing the listener, process, and recent logs |
| `-Action Uninstall` | Stops the supervisor and removes the shortcut and runtime directory without touching user configuration or the official package |

Common parameters:

- `-NoShortcut`: Do not create a Start menu shortcut during installation.
- `-DisableComputerUseBootstrap`: Do not write the Computer Use environment variable.
- `-StartupTimeoutSeconds <seconds>`: Changes the startup timeout from the default of 60 seconds.

---

## Smart Auto-Update

- **Silent Detection on Launch**: On startup, a lightweight background check queries GitHub for new releases (with a 2.5s timeout, silently skipped when offline). When a new version is detected, it is downloaded in the background to replace the executable and fires a desktop notification ("effective on next launch") without affecting startup latency.
- **Manual Online Update**:
  - macOS: `./codex-input-unlock update`
  - Windows: `.\codex-input-unlock.ps1 -Action Update`
- **Check Current Version**: `./codex-input-unlock version`

---

## Testing

```powershell
# Full static smoke test, including the configuration non-modification check
pwsh -NoProfile -ExecutionPolicy Bypass -File '.\tests\smoke.ps1'

# CI-friendly static test without requiring an installed Store package
pwsh -NoProfile -ExecutionPolicy Bypass -File '.\tests\smoke.ps1' -SkipPackageCheck
```

---

## Version adaptation

Codex releases re-minify and reshape the renderer bundle, so discovery uses **multiple anchors**:

1. **Semantic anchor:** a breakpoint inside the quota selector's `rate_limit?.allowed` guard treats quota data as available. It does not depend on composer component structure or minified names.
2. **Structural anchors:** locators for known composer submit-disabled chains (for example `composer-quota-consumer`) rewrite the gating variable to `false`.

At startup the supervisor arms every anchor found in the current bundle (up to four); any hit unlocks the composer. `breakpoints` and `gateStats.unlockCount` in `runtime-status-*.json` show which anchor actually fired. If a Codex update causes a startup timeout, the error carries `lastProbe` (quota atom count, presence of `submitDisabled`, per-locator matches). Run the self-test below before adding a rule:

```powershell
.\codex-input-unlock --self-test --bundle <extracted app-primary-*.js>
```

---

## Notes

1. **A cold start through the dedicated launcher is required.** The original official shortcut does not provide the CDP arguments.
2. **Only the local input gate is affected.** This tool does not fabricate server-side quota. Usage, authentication, and rate limits are controlled by your third-party provider.
3. **Unofficial project:** This project is independent and is not affiliated with OpenAI.

---

## Upstream & Acknowledgements

This repository is maintained as an independent fork of [1zero224/codex-input-unlock](https://github.com/1zero224/codex-input-unlock). Special thanks to the upstream author and community contributors for their initial work on CDP-based injection.

---

## License

[MIT](LICENSE)
