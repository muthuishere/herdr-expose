# 25. All plugin entrypoint ids are namespaced `hex:`

Status: Accepted

## Context

`herdr plugin action invoke <ACTION_ID>` takes `--plugin` as an **optional**
argument. So a bare action id like `open` is ambiguous the moment any other
installed plugin also defines `open` — and `open`, `status` and `pair` are
exactly the ids every plugin of this kind will want.

The failure is not an error message. It is the wrong plugin's action running.

## Decision

Every action, pane and link-handler id is prefixed **`hex:`** (hex =
herdr-expose):

```
hex:open · hex:pair · hex:status · hex:expose · hex:unexpose
hex:pair-qr (pane) · hex:status (pane) · hex:expose-url (link handler)
```

A `[[link_handlers]]` `action` field must reference the **prefixed** id.

**Verified against live Herdr 0.9.0** — linked, listed, invoked, with the command
log showing `succeeded` and real stdout:

- **`:` in an id is accepted.** So are `-` and `_`.
- **`.` in an id is rejected** with `invalid_plugin_action_id`. `hex.open` does
  not work, which rules out the other obvious namespacing convention.

## Consequences

- Every documented invocation is `herdr plugin action invoke hex:open`, not
  `open`. README and `docs/api.md` say so; a stale `open` in a doc is a bug.
- The ids are slightly uglier and completely unambiguous. Correct trade for a
  command a user types when something is already not working.
- The prefix is short deliberately — it is typed by hand.
- If Herdr later makes `--plugin` mandatory or auto-namespaces ids, this becomes
  redundant but stays harmless.
