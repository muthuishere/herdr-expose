## What changed

<!-- One or two sentences. If it touches the wire, the exposure ladder or share
     expiry, say so here explicitly. -->

## What you measured

<!-- Numbers, if this touches the streaming/fanout path or anything in SPEC A3 /
     AMENDMENTS 20. "A claimed number is not a number" — say what you ran, on
     what, and what it read. "N/A, no perf impact" is a fine answer. -->

## Gate you ran

- [ ] `go build ./... && go vet ./...`
- [ ] `go test -race ./...`
- [ ] `./e2e/acceptance.sh`
- [ ] `e2e/stress/` (required if this touches fanout or streaming)

## Spec / ADR

<!-- Which SPEC amendment or ADR this implements, or the new one you added if it
     changes a decision already recorded. -->
