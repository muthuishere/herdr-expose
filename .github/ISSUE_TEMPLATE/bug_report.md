---
name: Bug report
about: Something does not work
labels: bug
---

## `herdr-expose doctor --json`

<details><summary>doctor output</summary>

```json
PASTE HERE
```

</details>

This is the single most useful thing in the report. `doctor` checks every
external thing this binary depends on — the herdr binary and its PATH, the
session socket, the port and who holds it, the resolved exposure mode, the
tunnel, DNS from both a public resolver and this machine's, live shares and the
log — and it never prints a credential, so the output is safe to paste.

## Versions

- `herdr-expose --version`:
- `herdr --version`:
- OS and version:
- Browser and version (if the bug is in the web app):

## What you did

<!-- The exact command, including the exposure rung: bare (lan), --quick,
     --domain or --local. -->

## What you expected, and what happened instead

## Log

<!-- `herdr-expose logs` — the last ~50 lines around the failure. It never
     contains pane bytes; check anyway before pasting. -->
