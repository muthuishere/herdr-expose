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

As of 2026-09-20. This table is the claim. If a row says **not run on
hardware**, nothing else in this repo — README, badge, manifest — may imply
otherwise.

| # | Check | Status |
|---|---|---|
| 1 | `GOOS=windows GOARCH=amd64` and `GOOS=windows GOARCH=arm64`: `go build ./...` **and** `go vet ./...` | **Verified** — clean, and enforced in CI for all six targets |
| 2 | darwin and linux unaffected: `go build`, `go vet`, `go test -race ./...` | **Verified** — green throughout |
| 3 | Lock semantics (exclusive, non-blocking, released on close) | **Verified on Unix**, by a portable test that runs on every platform. The `LockFileEx` implementation is **not run on hardware** |
| 4 | The `\\.\pipe\` naming rule | **Confirmed from Herdr's published source** (`v0.9.1`: `session.rs`, `integration/env.rs`, `ipc.rs`, and five bundled integrations). **Not observed against a running Windows Herdr server** |
| 5 | `go-winio` dialling a real named pipe and round-tripping bytes | Test written (`//go:build windows`), **not run on hardware** |
| 6 | Job Object reaping a `cmd.exe` → `ping` grandchild with no orphan | Test written (`//go:build windows`), **not run on hardware** |
| 7 | Windows Service install / recovery actions / SCM stop | Implemented, **not run on hardware**. The test VM's agent session is unelevated, so the elevated path has never executed |
| 8 | `herdr-expose doctor`, `share --lan`, a browser driving a pane on Windows | **Not run on hardware** |

### Why not, specifically

Herdr **0.9.1 was installed successfully** on the test VM (Windows on ARM,
unelevated, via the documented `irm https://herdr.dev/install.ps1 | iex`
one-liner). Two facts came out of that and both belong here:

- **There is no native windows/arm64 Herdr build.** Release `v0.9.1` ships only
  `herdr-windows-x86_64.zip`, and the installer says so out loud: *"Windows
  ARM64 detected; installing the x86_64 build under Windows emulation."* So any
  future claim from that machine is "verified on Windows arm64 with Herdr
  running as x86_64 under emulation", not "verified on Windows".
- The remote-execution channel to that VM wedged before the checks could run,
  and recovering it needs someone at the machine. So rows 3 and 5–8 stayed
  unrun. They are written to be run, not aspirational: they are ordinary
  `go test` cases behind `//go:build windows`.

### The sentence we are entitled to publish

> **Windows: experimental.** Builds and vets clean for windows/amd64 and
> windows/arm64, and the platform-specific code (named pipe transport,
> `LockFileEx`, Job Objects, Windows Service) is implemented and unit-tested —
> but it has **not** been run on Windows hardware. Treat it as untested until
> this line says otherwise.

Nothing stronger. We removed ngrok this week rather than keep an implied
promise; this is the same rule applied to ourselves.


---

## Known gaps

- `internal/expose`'s test fixtures shell out to `sh -c`, so that package's
  suite is Unix-only at runtime. It cross-compiles and cross-vets on Windows;
  it does not run there. The tests that do run on Windows are in
  `internal/platform`, including `//go:build windows` cases for the pipe
  round-trip, the `\\.\pipe\` naming table, and a Job Object reaping a
  grandchild.
- `internal/config` still derives its config directory the Unix way, so
  `config.toml` lands under `C:\Users\<you>\.config\herdr-expose` rather than
  `%APPDATA%`. It works; it is not idiomatic. That package was out of scope for
  this port.
- There is no Windows runner in the release workflow. The floor CI enforces is
  cross-build **and cross-vet** of all six targets, which catches a Unix-only
  syscall landing in a shared file — it does not catch a runtime bug.
- `config_cmd.go`'s editor launch and `logging.go`'s paths were not audited for
  Windows beyond compiling.
