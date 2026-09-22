# Outbound writer and review runs: implementation plan

**Goal:** every agent message leaves through the daemon's writer with validation
and a receipt; committee verdicts are aggregated from structured results.

**Architecture:** `handoffd post` writes JSON records into an outbox
directory; a sequential pipeline worker validates each record against thread
state and the review run, delivers to Slack (helper or built-in client) or the
MR (`glab`/`gh`), writes a receipt and journal entry, and updates the run.
Review rounds live in the thread record; a complete round triggers a results
continuation to the driver.

**Spec:** `docs/superpowers/specs/2026-09-13-outbound-writer-and-review-runs-design.md`

**Constraints:** Go stdlib only; tests and lint in Docker
(`golang:1.25`, `golangci/golangci-lint:v2.5.0`); no comments in code; no
commits until asked.

## File map

- `internal/outbox/outbox.go` (new): `Record`, `Receipt`, `Kind`, `Dir` with
  `Write`, `Pending`, `WriteReceipt`, `ReadReceipt`, `Quarantine`, `Sweep`,
  `Retry`; `Wait`.
- `internal/state/state.go`: `Run`, `Result`, `Joint`; `Thread.Run`,
  `Thread.RunsHistory`; `Thread.OpenRound`, `Thread.Agents`.
- `internal/journal/journal.go`: `ActionPosted`, `ActionRejected`,
  `SourceOutbox`, `Entry.Round`.
- `internal/slackfetch/slackfetch.go`: helper-backed `Post`.
- `internal/slackapi/slackapi.go`: `Post` via `chat.postMessage`.
- `internal/forge/forge.go`: `Note(ctx, mr, text)`.
- `internal/config/config.go`: `PostLabel`, `Label()`, allow/deny defaults.
- `internal/prompt/prompt.go`, `templates/{ru,en}/{initial,continuation,results}.tmpl`:
  `Context.PostBin`, `Context.Round`, `Context.NewRound`; `Results`.
- `internal/pipeline/outbox.go` (new): worker, validation, delivery, run updates.
- `internal/pipeline/{launch,deliver,workspace,progress}.go`: open round 1,
  detect head move, results continuation, shared progress recording.
- `internal/watchdog/watchdog.go`: collected-without-joint alert.
- `cmd/handoffd/postcmd.go` (new), `main.go`: `post` command, wiring,
  outbox sweep and replay.

## Tasks

1. **outbox package.** Records atomic (tmp+rename), `Pending` lists records
   without receipts sorted by `At`, `Quarantine` moves malformed files to
   `rejected/`, `Retry` drops `pending` receipts, `Sweep` removes old pairs,
   `Wait` polls for a receipt. Tests for each.
2. **State and journal.** `Run` types, `OpenRound`, JSON round-trip test;
   journal constants and `Round` field; `Outcomes` treats `posted` markers the
   same as `progress` (both carry `Phase`).
3. **Delivery clients.** `slackfetch.Fetcher.Post` (helper `--post`),
   `slackapi.Client.Post` (httptest), `forge.Client.Note` (`glab mr note`,
   `gh pr comment`).
4. **Config.** `post_label`, `Label()`, new allow/deny defaults, validation
   that label is non-empty when persona agent is set; tests.
5. **Writer worker.** `pipeline/outbox.go`: `Outbox`, `Poster`, `Notes` deps;
   `RunOutbox(ctx)`; `handleRecord`: unknown thread, foreign session, sha
   mismatch (re-read head for review_done), marker order, duplicate, label in
   text; delivery with three attempts and `pending` receipt plus alert on
   failure; journal entries; progress recording on markers; results
   collection; joint verdict gate; results continuation to the driver via
   `Results` template. Tests for every rule.
6. **Rounds in the main flow.** `launch` opens round 1 with `ws.primary`
   head/base; `syncWorktree` opens a new round when the inspected head differs
   from the run head, continuation prompt carries `Round`/`NewRound`;
   single review uses the same run. Tests.
7. **Prompts.** Protocol rewritten to `post` commands in `ru`/`en` initial and
   continuation; committee blocks updated; `results.tmpl`; prompt tests.
8. **CLI and wiring.** `postcmd.go`: flags, record, wait, exit codes; `main.go`
   wires outbox dir, poster, notes, `RunOutbox`, sweep, replay retry;
   watchdog alert for collected-without-joint. README, wiki.
