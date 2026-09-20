# Windows

**Status: experimental.** Read the [What we claim](#what-we-claim) table before
you rely on this. It says, check by check, what has been run on real Windows
hardware and what has only been cross-compiled.

herdr-expose was Unix-only until this port. Herdr itself supports Windows, so
the gap was ours, not Herdr's.

---

## Install

```powershell
herdr plugin install muthuishere/herdr-expose
```

With Go and node present that builds from source; without them it falls back to
the published `windows_amd64` / `windows_arm64` release asset and verifies it
against the release `SHA256SUMS`.

`scripts/build.sh` is a POSIX shell script, so the from-source path needs a
POSIX shell — Git Bash or MSYS2. The script detects `MINGW*` / `MSYS*` /
`CYGWIN*` from `uname -s` and produces `bin\herdr-expose.exe`. It does **not**
symlink into `~/.local/bin` on Windows, because that convention does not exist
there; it prints the directory to add to `PATH` instead.

The manifest still spells the command `./bin/herdr-expose`. That is correct:
`CreateProcess` appends `.exe` when the command has no extension at all.

---

## How the port is built

Every OS-specific thing lives in `internal/platform/`, in build-tagged pairs —
never a `runtime.GOOS` branch at the call site. Each platform's implementation
reads on its own, and the Unix path was moved verbatim rather than reshaped to
accommodate Windows.

| Concern | Unix | Windows |
|---|---|---|
| Control socket | `net.Dial("unix", …)` | `go-winio` `DialPipeContext` |
| Advisory lock | `flock(LOCK_EX\|LOCK_NB)` | `LockFileEx(EXCLUSIVE\|FAIL_IMMEDIATELY)` |
| Process tree | `setpgid` + `kill(-pgid)` | Job Object, `KILL_ON_JOB_CLOSE` |
| Detached child | `setsid` | `DETACHED_PROCESS` |
| Liveness | `kill(pid, 0)` | `OpenProcess` + `GetExitCodeProcess` |
| Command lines | `ps -axo pid=,command=` | `Get-CimInstance Win32_Process` |
| Supervision | launchd / systemd `--user` | Windows Service (SCM) |
| State dir | `~/.local/state/herdr-expose` | `%LOCALAPPDATA%\herdr-expose\state` |
| Config dir | `~/.config/herdr-expose` | `%APPDATA%\herdr-expose` |
| Executable test | `mode & 0o111` | file extension vs `PATHEXT` |

### The named pipe

This is the part most likely to be got wrong, so here is the evidence rather
than an assertion.

Herdr sets `HERDR_SOCKET_PATH` to a **filesystem-shaped path on every
platform** — `session.rs` builds `<config_dir>/herdr.sock` (or
`<config_dir>/sessions/<name>/herdr.sock`) with no Windows branch, and
`integration/env.rs` exports that value verbatim. The Windows-ness lives
entirely in the client. Herdr's own bundled agent integrations, five of them,
spell it identically:

```js
const socketPath = process.env.HERDR_SOCKET_PATH;
const socketEndpoint =
  process.platform === "win32" ? `\\\\.\\pipe\\${socketPath}` : socketPath;
```

and the server side agrees: `ipc.rs` hands the same string to `interprocess`'s
`GenericNamespaced`, which prepends the same prefix. So the live pipe is really
named:

```
\\.\pipe\C:\Users\me\AppData\Roaming\herdr\herdr.sock
```

That looks wrong and is right. A pipe name is an opaque string inside the pipe
namespace, and `:` and `\` are legal in it after the prefix.

`platform.ControlEndpoint` therefore prefixes the **whole** value, and is
tolerant of a value that already names the namespace so a future Herdr which
exports the fully-qualified form would still work.

One more consequence, and a welcome one: `ipc.rs` also writes a **marker file**
at the socket path on Windows, for stale-server detection. So the filesystem
session scan in `internal/upstream/registry.go` works there unchanged, and only
the dial had to change. The config root it scans does move —
`%APPDATA%\herdr`, not `~/.config/herdr`.

### Why a Windows Service, and not a Scheduled Task

`service install` means one thing on Unix: **survive a reboot and survive a
crash**. launchd `KeepAlive` and systemd `Restart=always` both deliver exactly
that. A logon-triggered Scheduled Task does not — it starts only after a human
logs in, and its failure handling is a retry count, not a supervisor. Shipping
the same verb with a materially weaker promise on one platform is how somebody
ends up believing their server is supervised when it is not.

So the default is a real Windows Service: SCM auto-start at boot with no logon,
`SetRecoveryActions` set to restart forever, and `service status` reading live
state out of the SCM. That needs Administrator, which is a real cost, so:

- **without elevation, `service install` refuses** and prints how to elevate. It
  does not quietly do something lesser.
- `service install --task` is the named opt-out: a logon-triggered Scheduled
  Task, no admin, and the message says what it gives up.

Because the SCM starts a service with a control channel instead of a command
line, `main()` hands control to `svc.Run` when it detects it was started by the
SCM, and a `SERVICE_CONTROL_STOP` cancels the same context a Ctrl-C does — so
tunnel teardown and share revocation run on a service stop instead of being
dead code.

### Why a Job Object, and not `taskkill /T`

`CREATE_NEW_PROCESS_GROUP` only scopes console Ctrl events; killing a pid on
Windows leaves its children running. `cloudflared` spawns children, and an
orphaned one holds a tunnel open against a hostname we believe we tore down —
the exact hazard the Unix stray-reclaim ledger exists for.

A Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` is the only construct
that guarantees the tree dies, **including when herdr-expose is killed and never
runs cleanup at all**. That is the Windows counterpart of "the kernel drops the
flock when the holder dies".

Honest caveat: `exec.Cmd` cannot create a child suspended, so there is a
sub-millisecond window between `CreateProcess` and `AssignProcessToJobObject`
in which a grandchild could escape. `KillTree` sweeps by command line afterwards
as a backstop.

---

## What we claim

Verified 2026-09-20 on a real Windows-on-ARM machine (Parallels), with a
**native windows/arm64** herdr-expose talking to **Herdr 0.9.1 running as
x86_64 under Windows emulation** — there is no native arm64 Herdr build, and
the installer says so out loud.

| # | Check | Result |
|---|---|---|
| 1 | `go build` + `go vet`, windows/amd64 and windows/arm64 | **PASS** — and enforced in CI for all six targets |
| 2 | darwin/linux unaffected: build, vet, `go test -race` | **PASS** — green throughout |
| 3 | `internal/platform` suite, run natively on windows/arm64 | **PASS** — 13/13 |
| 4 | The `\\.\pipe\` naming rule, against a **live Herdr server** | **PASS** — see below |
| 5 | `go-winio` dialling the real Herdr pipe and round-tripping a request | **PASS** — `herdr socket … herdr 0.9.1, protocol 22` |
| 6 | Job Object reaping a `cmd.exe` → `ping` tree, no orphan | **PASS** — `TestKillTreeReapsAGrandchild`, plus `KILL_ON_JOB_CLOSE` on handle close |
| 7 | `LockFileEx`: two `serve` launched simultaneously | **PASS** — one process, one listener on 21118; the second exited silently |
| 8 | State / config paths | **PASS** for state (`%LOCALAPPDATA%\herdr-expose\state`); config is a known gap, below |
| 9 | `herdr-expose doctor` | **PASS** — every check, "everything that must work, works" |
| 10 | `serve` + embedded web UI over HTTP | **PASS** — `/healthz` `{"ok":true,"upstream":true,"sessions_connected":1,"web_ui":true}` |
| 11 | A real browser (Edge headless) rendering the UI | **PASS** — title `Herdr Expose`, app root present, its JS ran |
| 12 | `share --lan` | **PASS** — scoped instance, pairing QR, serving on the LAN IP |
| 13 | `service install` unelevated | **PASS** — refuses with instructions, exit 1. **The elevated path is still unrun** |
| 14 | **End to end: Claude Code on the VM uses the `herdr-share` skill to share its own session** | **PASS** — raised a live LAN share, and a browser rendered it |

### The named pipe, confirmed against reality

`herdr session list --json` on the box reports

```
socket_path = C:\Users\muthuishere\AppData\Roaming\herdr\herdr.sock
```

and enumerating the pipe namespace returns exactly:

```
C:\Users\muthuishere\AppData\Roaming\herdr\herdr.sock
C:\Users\muthuishere\AppData\Roaming\herdr\herdr-client.sock
```

So the pipe really is named `\\.\pipe\` + the whole socket path, as the source
reading predicted. `herdr-expose doctor` then dialled it and got
`herdr 0.9.1, protocol 22` back. The rule is no longer an inference.

### Two bugs this found that compiling never would

1. **`LockFileEx` is MANDATORY where `flock` is advisory.** Locking byte 0 of
   the pidfile made `herdr-expose status` print `pid not running` while the
   daemon was serving happily — our own `os.ReadFile` was failing with
   `ERROR_LOCK_VIOLATION` against our own lock. Fixed by locking a byte at
   offset 1<<62, past any content, which restores flock semantics exactly.
   Regression test: `TestLockedFileIsStillReadable`.
2. **`os.Symlink` needs a privilege an ordinary user does not have.**
   `skill install` died with "A required privilege is not held by the client"
   unless Developer Mode was on. Fixed with a **directory junction** fallback
   (`mklink /J`, no privilege required) — and then a second bug behind it: Go
   reports a junction as a plain directory, so `skill status` disowned its own
   link, reinstall refused, and uninstall would not remove it. Both fixed by
   asking for `FILE_ATTRIBUTE_REPARSE_POINT`.

### The install story is the weak part

`herdr plugin install muthuishere/herdr-expose` — the command this document
tells a user to run — **fails on a stock Windows box**:

```
Error: Error { kind: NotFound, message: "program not found" }
```

Herdr shells out to `git` to clone, and the test machine had no `git`. It also
had no `bash` and no `curl`, which means **both** halves of `scripts/build.sh`
are unavailable: the from-source path needs a POSIX shell, and the release-asset
fallback needs `curl`. Everything above was therefore verified with a
hand-built binary copied into place.

So, plainly: **on Windows this currently installs only if you hand-build and
hand-copy.** Making `herdr plugin install` work there needs the build hook to
run without bash — the manifest takes a single `command`, so that is a real
design question, not a one-line fix — and a published windows asset to fall
back to.

### The sentence we are entitled to publish

> **Windows: experimental, and verified end to end on Windows arm64** (with
> Herdr 0.9.1 itself running as x86_64 under emulation) — the named-pipe
> transport, file locking, Job Object process-tree kill, `doctor`, `serve`, the
> web UI, `share --lan`, and an agent using the `herdr-share` skill to share its
> own session all work. **Two things are not proven:** `service install` as an
> elevated Windows Service, and `herdr plugin install`, which needs `git` and a
> POSIX shell the platform does not ship. Install by hand for now.

## Known gaps

- `internal/expose`'s test fixtures shell out to `sh -c`, so that package's
  suite is Unix-only at runtime. It cross-compiles and cross-vets on Windows;
  it does not run there. The tests that do run on Windows are in
  `internal/platform`, including `//go:build windows` cases for the pipe
  round-trip, the `\\.\pipe\` naming table, and a Job Object reaping a
  grandchild.
- `internal/config` still derives its config directory the Unix way, so
  `config.toml` lands under `C:\Users\<you>\.config\herdr-expose` rather than
  `%APPDATA%`. Confirmed on hardware; it works, it is just not idiomatic. That
  package was out of scope for this port.
- `service install` as an actual elevated Windows Service is **unrun**. The
  unelevated refusal is verified; the SCM registration, recovery actions and
  `svc.Run` handler are not.
- `herdr plugin install` does not work on Windows — see "The install story is
  the weak part" above.
- There is no Windows runner in the release workflow. The floor CI enforces is
  cross-build **and cross-vet** of all six targets, which catches a Unix-only
  syscall landing in a shared file — it does not catch a runtime bug.
- `config_cmd.go`'s editor launch and `logging.go`'s paths were not audited for
  Windows beyond compiling.
