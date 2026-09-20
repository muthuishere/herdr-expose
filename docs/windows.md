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
| Supervision | launchd / systemd `--user` | **nothing installed** — see below |
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

### Why Windows installs no autostart at all

`service install` does nothing on Windows. Not a Windows Service, not a
Scheduled Task, not a Startup-folder entry. It prints what to run and exits 0.

That is a deliberate asymmetry, and it is worth stating why, because Windows can
run services perfectly well and the next person to read this will assume we
simply never got round to it.

**The rule is what the platform is USED as, not what the OS can do.**

- **Linux is frequently a server** — a box you leave running that has to come
  back by itself after a reboot with nobody logged in. `systemd --user` with
  `Restart=always` genuinely earns its place.
- **macOS is the dev machine people leave open all day**, so launchd
  `KeepAlive` earns its place too.
- **Windows, here, is somebody's desktop** — they are sitting in front of it,
  with a terminal already open. A user who is physically at the machine can run
  one command. A headless Linux host cannot.

We also tried the alternatives and all three were worse than they looked:

- The **SCM Service** runs as LocalSystem, which resolves `%APPDATA%` to
  `C:\Windows\System32\config\systemprofile` and therefore finds **zero** of
  the user's Herdr sessions — it would supervise an empty UI. Measured: it could
  not even find the herdr binary (Herdr installs onto the *user* PATH), and the
  SCM reported *"terminated with the following error: Incorrect function."* It
  also demanded Administrator, which neither macOS nor Linux does.
- The **Scheduled Task** was refused outright on the test machine:
  `schtasks /Create` → `ERROR: Access is denied.` for a non-admin.
- The **Startup folder** does not supervise anything — it starts us at logon and
  that is all.

Three mechanisms, each with its own install, status, uninstall and failure
modes, on the one platform none of us runs daily. That is surface area, not
sophistication, so it was deleted rather than left dormant behind a flag.

### What actually happens when the process dies

| | starts automatically | restarts on crash |
|---|---|---|
| **macOS** | LaunchAgent at login, **plus** Herdr's plugin startup hook | **yes** — launchd `KeepAlive` |
| **Linux** | systemd `--user` at boot (with lingering), **plus** the startup hook | **yes** — `Restart=always` |
| **Windows** | Herdr's plugin startup hook only, when Herdr starts | **no** |

The Windows row is measured, not assumed. Starting the Herdr server on the test
machine with nothing of ours running produced, from Herdr's own plugin log:

```json
{"command":["./bin/herdr-expose","daemon"],"event":"startup","exit_code":0,
 "status":"succeeded","stdout":"started, pid 12892\n..."}
```

and `/healthz` answered 200 immediately after. So the hook genuinely starts us
on Windows; it is not a convenient assumption.

Windows keeps two of the three supervision layers: Herdr's `[[startup]]` hook
fork-execs `herdr-expose daemon` (layer 1, and it is per-user and needs no
privilege on every platform), and the managed-pid ledger with `reclaimStrays`
still prevents two servers fighting for one port (layer 3). What it does not
have is layer 2, the restart-on-crash supervisor.

**So when it is not running, we say so.** `herdr-expose open` — the `hex:open`
action and the URL link handler, i.e. the moment a user reaches for the UI —
checks the pidfile and, if nothing is serving, surfaces one line through
Herdr's own `notification show`, naming the command:

```
herdr-expose is not running — start it with `herdr-expose daemon`
```

It is user-initiated, never on a timer, and silent whenever the server is up.
On macOS and Linux it is a no-op: a supervisor has already restarted the server
by the time anyone could read a notification about it.

Verified on hardware: with nothing serving, `open` issued the `notification.show`
call and Herdr's server log recorded it `outcome="ok"`; with the daemon running,
`open` made **no API call at all** and printed no prompt. One caveat worth
knowing — Herdr can have notifications switched off, in which case the API
answers `{"reason":"disabled","shown":false}` and nothing is displayed. That is
the user's setting, not a failure, and it is why the same line is ALSO written
to stderr, where it is unconditional.

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
| 13 | `service install` | Now installs **nothing** by design and says what to run. Verified on hardware that the three mechanisms it replaced were each unusable: Service fails as LocalSystem, `schtasks /Create` → `Access is denied.`, Startup folder does not supervise |
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

### Bugs this found that compiling never would

1. **`LockFileEx` is MANDATORY where `flock` is advisory.** Locking byte 0 of
   the pidfile made `herdr-expose status` print `pid not running` while the
   daemon was serving happily — our own `os.ReadFile` was failing with
   `ERROR_LOCK_VIOLATION` against our own lock. Fixed by locking a byte at
   offset 1<<62, past any content, which restores flock semantics exactly.
   Regression test: `TestLockedFileIsStillReadable`.
2. **A teardown check that asked once instead of waiting.** `share revoke --all`
   printed `port STILL BOUND` and exited 1 on a share that had torn down
   perfectly, because it polled the port microseconds after the holding process
   died — and a listening socket is not closed synchronously with its process.
   Windows made it reproducible: the share is stopped with `TerminateProcess`
   (there is no signal it can handle), and Go does not set `SO_REUSEADDR` there,
   so a port a browser had just used sat in `TIME_WAIT`. Fixed by giving the
   check a 5s window, and by asking "is anything SERVING" rather than "can I
   bind" on Windows (`platform.PortBindable`).
3. **A skill link into a directory that was about to be moved.** Herdr builds a
   plugin in a STAGING directory (`…\plugins\.tmp-install-<pid>-<ts>\checkout`)
   and moves it to its final home only after the build hook succeeds. Our build
   script runs `skill install` as its last step, so the link pointed into the
   staging path and Herdr then moved the checkout out from under it: the install
   reported success, `~\.claude\skills\herdr-share` existed, and every read
   through it failed. The agent skill was installed **broken**. Fixed by healing
   a DANGLING link on `daemon` start — the startup hook is the first thing that
   runs from the final location, so it is the first moment the real path is
   knowable.
4. **`os.Symlink` needs a privilege an ordinary user does not have.**
   `skill install` died with "A required privilege is not held by the client"
   unless Developer Mode was on. Fixed with a **directory junction** fallback
   (`mklink /J`, no privilege required) — and then a second bug behind it: Go
   reports a junction as a plain directory, so `skill status` disowned its own
   link, reinstall refused, and uninstall would not remove it. Both fixed by
   asking for `FILE_ATTRIBUTE_REPARSE_POINT`.

### The install story

`herdr plugin install muthuishere/herdr-expose` **works on Windows**, verified
from a wiped machine against the published repo:

```
build commands: 2
  build (skipped on windows): ./scripts/build.sh
  build: powershell -NoProfile -ExecutionPolicy Bypass -File ./scripts/build.ps1
Installed dev.deemwar.herdr-expose from muthuishere/herdr-expose.
exit=0 in 51.6s
```

and `bin\herdr-expose.exe` was really there afterwards.

**It requires `git`** — not only to clone (Herdr shells out to it, and without
it the install dies with `Error { kind: NotFound, message: "program not found" }`)
but as the thing that makes the rest reachable at all. Note what it does **not**
require: `bash`. A default Git for Windows install puts only `Git\cmd` on PATH,
so `bash.exe` in `Git\bin` is **not** reachable — measured, not assumed — which
is exactly why the build hook is a PowerShell script selected by the per-entry
`platforms` filter on `[[build]]` rather than the `.sh` the other platforms use.
`CreateProcess` cannot execute a `.sh` at all: there is no shebang handling,
so even a bash on PATH would not have helped.

Before that filter existed, a Windows install printed
`build (skipped on windows)`, **reported success, and built nothing** — leaving
the plugin enabled with every action pointing at a `./bin/herdr-expose` that did
not exist. That is the failure mode this section exists to prevent recurring.

### The sentence we are entitled to publish

> **Windows: experimental, and verified end to end on Windows arm64** (with
> Herdr 0.9.1 itself running as x86_64 under emulation) — the named-pipe
> transport, file locking, Job Object process-tree kill, `doctor`, `serve`, the
> web UI, `share --lan`, and an agent using the `herdr-share` skill to share its
> own session all work. Windows installs **no autostart service**: Herdr's own
> plugin startup hook starts the daemon, nothing restarts it if it crashes, and
> herdr-expose tells you the command when you reach for the UI and it is down.
> `herdr plugin install` works on Windows and requires **git** (for the clone);
> it does **not** require bash.

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
- **No restart-on-crash on Windows.** This is a design decision, not a gap —
  see "Why Windows installs no autostart at all" — but it is a real difference
  from macOS and Linux and the table above states it exactly.
- The version string is stamped `dev` on a Windows plugin install, because the
  clone Herdr makes carries no tags and `git` is not on PATH in the environment
  it runs the build hook in. Cosmetic, but it means `herdr-expose version` does
  not identify the build.
- There is no Windows runner in the release workflow. The floor CI enforces is
  cross-build **and cross-vet** of all six targets, which catches a Unix-only
  syscall landing in a shared file — it does not catch a runtime bug.
- `config_cmd.go`'s editor launch and `logging.go`'s paths were not audited for
  Windows beyond compiling.
