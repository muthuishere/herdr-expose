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

### On Windows, install NO autostart — and why that is not an inconsistency

macOS installs a LaunchAgent. Linux installs a systemd `--user` unit. Windows
installs nothing, prints the command, and exits 0.

Somebody will eventually read that and conclude we ran out of time, because
Windows runs services perfectly well. So the rule is written down here:

> **Supervision follows what the platform is USED as, not what the OS can
> technically do.**

- **Linux is frequently a server**: left running, must come back after a reboot
  with nobody logged in. `Restart=always` earns its place.
- **macOS is the dev machine left open all day.** `KeepAlive` earns its place.
- **Windows, for this product, is somebody's desktop** — they are sitting in
  front of it with a terminal open. A person at the machine can run one command;
  a headless Linux host cannot.

The empirical half matters too, because all three Windows mechanisms were tried
on real hardware and each was worse than it looked:

- an **SCM Service** runs as LocalSystem, resolves `%APPDATA%` to the system
  profile, and therefore finds ZERO of the user's Herdr sessions — it would
  supervise an empty UI. It also could not find the herdr binary (user PATH) and
  the SCM reported "terminated with the following error: Incorrect function". And
  it demanded Administrator, which neither of the other platforms does.
- a **Scheduled Task** was refused outright: `schtasks /Create` →
  "ERROR: Access is denied." for a non-admin.
- the **Startup folder** starts us at logon and supervises nothing.

Carrying three mechanisms — each with its own install, status, uninstall and
failure modes — on the one platform none of us runs daily is surface area, not
sophistication. They are deleted, not parked behind a flag: dead code that can
only run where nobody exercises it is the worst kind to keep.

What Windows keeps is layers 1 and 3: Herdr's own `[[startup]]` hook fork-execs
`herdr-expose daemon` (per-user, no privilege, every platform), and the
managed-pid ledger still stops two servers fighting for a port. The missing
layer is 2, restart-on-crash. So `herdr-expose open` checks the pidfile and, if
nothing is serving, surfaces ONE line through Herdr's own `notification show`
naming the command. User-initiated, never on a timer, silent while the server is
up, and a no-op on macOS/Linux where a supervisor has already fixed it.

## Consequences

- Two new dependencies, both Windows-only at link time:
  `github.com/Microsoft/go-winio` and `golang.org/x/sys/windows`. (The
  `windows/svc`, `svc/mgr` and `registry` subpackages were used only by the
  deleted Service, and are gone with it.)
- `go.mod` pins `golang.org/x/sys v0.47.0`, the newest release that still
  declares `go 1.25.0`. v0.48.0 requires `go 1.26.0` and would have bumped this
  module's toolchain floor as a side effect of a Windows port.
- CI cannot run Windows tests, so the workflow cross-**builds and cross-vets**
  all six targets. That catches a Unix-only syscall landing in a shared file. It
  does not catch a runtime bug, and `docs/windows.md` says so.
- `internal/expose`'s fixtures shell out to `sh -c`, so that suite stays
  Unix-only at runtime. The tests that run on Windows live in
  `internal/platform`.
- Three bugs were found by doing this rather than by compiling, and only one of
  them was visible without running on hardware:
  1. `isExec` tested `mode & 0o111`, which is **always false on Windows** —
     `os.Stat` there has no execute bit. A direct port would have silently
     disabled the binary-search fallback, which is precisely the path a Windows
     Service depends on, since a service inherits the machine PATH.
  2. **`LockFileEx` is mandatory where `flock` is advisory.** Locking byte 0 of
     the pidfile made `status` report `pid not running` against a live daemon,
     because reading our own pidfile failed with `ERROR_LOCK_VIOLATION`. The
     lock now sits at offset 1<<62, past any content, which is what restores
     flock's semantics.
  3. **`os.Symlink` needs a privilege ordinary users lack**, so `skill install`
     could not run without Developer Mode. It now falls back to a directory
     JUNCTION — and, because Go reports a junction as a plain directory, the
     link *classifier* had to learn `FILE_ATTRIBUTE_REPARSE_POINT` too, or
     status/uninstall/idempotency all disown a link we just made.
- Not solved here, and named in `docs/windows.md`: `herdr plugin install` does
  not work on Windows. Herdr shells out to `git`, and `scripts/build.sh` needs a
  POSIX shell for the source path and `curl` for the release fallback. A stock
  Windows box has none of the three. The manifest allows a single `[[build]]`
  command, so making that work is a design question, not a patch.
