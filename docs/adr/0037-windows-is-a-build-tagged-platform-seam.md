# 37. Windows is a build-tagged platform seam, not a `runtime.GOOS` branch

Status: Accepted (adds `internal/platform`; changes nothing about the Unix
behaviour of 0001's build contract or 0025's plugin entrypoints)

## Context

herdr-expose did not compile for `GOOS=windows`. Herdr does support Windows, so
the gap was ours. Six things were in the way, and they are six different
problems wearing one label:

1. `net.Dial("unix", …)` against `$HERDR_SOCKET_PATH` — Herdr uses a **named
   pipe** on Windows.
2. `syscall.Flock` on the pidfile, the share record and the share run lock —
   undefined on Windows.
3. `SysProcAttr.Setpgid`, which is how a tunnel's whole process tree is killed.
4. `SysProcAttr.Setsid`, which is how the daemon and a share detach.
5. `kill(pid, 0)`, `SIGTERM`, `SIGKILL`, and `ps -axo` — no signals, no `ps`.
6. launchd / systemd, with no Windows equivalent wired.

The tempting shape is `if runtime.GOOS == "windows"` at each call site. That
buys a small diff and pays for it forever: the Unix path — the one that has been
carrying the owner's live sessions — gets threaded with branches that only exist
for a platform it never runs on, and neither implementation can be read on its
own.

## Decision

**One package, `internal/platform`, with build-tagged file PAIRS: one concept
per pair, a small shared signature, and no `runtime.GOOS` at any call site.**

The Unix implementations were moved across **verbatim**, so the port cannot have
changed the behaviour of the platform that was already working. Likewise the
launchd/systemd code moved into `service_unix.go` unaltered.

Three of the six deserve their reasoning recorded, because the obvious answer is
wrong in each case.

### The pipe name is the whole socket path

Herdr sets `HERDR_SOCKET_PATH` to a filesystem-shaped path on **every**
platform; there is no Windows branch in `session.rs` or `integration/env.rs`.
The Windows-ness lives in the client, and Herdr's own five bundled integrations
all do `` `\\\\.\\pipe\\${socketPath}` ``. The server agrees: `ipc.rs` hands the
same string to `interprocess`'s `GenericNamespaced`. So the real pipe is named
`\\.\pipe\C:\Users\me\AppData\Roaming\herdr\herdr.sock` — which looks wrong and
is right, because a pipe name is an opaque string in the pipe namespace.

We prefix the **whole** value, and tolerate a value that already names the
namespace so a future Herdr exporting the qualified form still works.

A happy consequence: `ipc.rs` also writes a marker FILE at that path, so the
filesystem session scan works on Windows unchanged. Only the dial moved.

### A Job Object, not `taskkill /T`

`CREATE_NEW_PROCESS_GROUP` only scopes console Ctrl events. Killing a pid on
Windows leaves its children running, and `cloudflared` has children. An orphan
holds a tunnel open against a hostname we believe we tore down — the hazard the
Unix stray-reclaim ledger already exists for.

`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` is the only construct that guarantees the
tree dies **including when herdr-expose is killed and never runs cleanup**. It
is the exact counterpart of "the kernel drops the flock when the holder dies",
which is the property the locks are built on. `exec.Cmd` cannot create a child
suspended, so a grandchild could escape the sub-millisecond window before
`AssignProcessToJobObject`; `KillTree` sweeps by command line as a backstop.

### A Service, not a Scheduled Task

`service install` means "survive a reboot and survive a crash" — that is what
launchd `KeepAlive` and systemd `Restart=always` deliver. A logon-triggered
Scheduled Task starts only after a human logs in and has a retry count rather
than a supervisor. The same verb making a materially weaker promise on one
platform is how somebody comes to believe their server is supervised when it is
not.

So: a real Windows Service, with SCM failure actions set to restart forever.
That needs Administrator, so **without elevation `service install` refuses** and
says how to elevate, and `--task` is a named opt-out that states what it gives
up. Because the SCM starts a service with a control channel and not a command
line, `main()` hands off to `svc.Run` when it detects the SCM started it, and a
`SERVICE_CONTROL_STOP` cancels the same context Ctrl-C does — otherwise tunnel
teardown and share revocation would be dead code on Windows.

## Consequences

- Two new dependencies, both Windows-only at link time:
  `github.com/Microsoft/go-winio` and `golang.org/x/sys/windows`.
- `go.mod` pins `golang.org/x/sys v0.47.0`, the newest release that still
  declares `go 1.25.0`. v0.48.0 requires `go 1.26.0` and would have bumped this
  module's toolchain floor as a side effect of a Windows port.
- CI cannot run Windows tests, so the workflow cross-**builds and cross-vets**
  all six targets. That catches a Unix-only syscall landing in a shared file. It
  does not catch a runtime bug, and `docs/windows.md` says so.
- `internal/expose`'s fixtures shell out to `sh -c`, so that suite stays
  Unix-only at runtime. The tests that run on Windows live in
  `internal/platform`.
- One real bug was found by doing this rather than by compiling: `isExec` tested
  `mode & 0o111`, which is **always false on Windows** — `os.Stat` there has no
  execute bit. A direct port would have silently disabled the binary-search
  fallback, which is precisely the path a Windows Service depends on, since a
  service inherits the machine PATH and not the user's.
