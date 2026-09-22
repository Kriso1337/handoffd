# Outbound writer and review runs

Date: 2026-09-13. Based on an architecture review of outbound delivery and
review aggregation.

## Goal

Every message an agent sends outward (Slack thread reply, MR comment, review
marker) passes through one writer inside the daemon that validates it against
the thread state, the current review round and the MR head, delivers it, and
leaves a receipt. Committee verdicts are aggregated from structured results
collected by the daemon, not from free text in Slack.

## Out of scope

Inline MR discussions with diff positions, editing sent messages, a writer for
help windows, automatic synthesis of the joint verdict without the driver.

## 1. Outbox protocol

`handoffd post` writes one JSON record to
`~/.local/state/handoffd/outbox/<id>.json` and waits up to 60 s for
`outbox/<id>.receipt.json`.

```
handoffd post --thread <root_ts> "text"
handoffd post --thread <root_ts> --mr-note "text"
handoffd post --thread <root_ts> --marker taken|review-start --sha <sha>
handoffd post --thread <root_ts> --review-done --sha <sha> --blockers N --others M [--decision "..."]
```

Record: `id`, `at`, `thread_ts`, `kind` (`reply | mr_note | marker | review_done`),
`text`, `sha`, `phase` (for markers), `blockers`, `others`, `decision`,
`session_id` (from `HANDOFFD_SESSION_ID`). The agent name is resolved by the
daemon from the session id, never from an argument.

Exit codes: 0 `posted <permalink>`, 1 `rejected: <reason>`, 2 `pending`
(no receipt in time; the daemon still delivers or dead-letters it).

Records are written atomically (tmp + rename). The daemon polls the outbox
directory once a second and processes leftovers at start. Receipts live next
to records; both are removed by the hourly sweep together with thread records.

Permissions. Reviewer allow-list gains `Bash(handoffd post:*)` and loses
`Bash(glab mr note:*)`, `Bash(glab api:*)`, `Bash(gh pr comment:*)`. Reviewer
deny-list gains `mcp__slack-agent-bridge__post_to_agent_channel`,
`mcp__slack-agent-bridge__edit_agent_channel_message`, `Bash(glab mr note:*)`,
`Bash(glab api:*)`, `Bash(gh pr comment:*)`, `Bash(gh api:*)`. Help windows
keep the bridge. Under Codex `--sandbox read-only` writing the outbox record
asks for approval; that is the single manual gate for a Codex reviewer.

## 2. Writer

One sequential worker in the pipeline. For each record, checks in order; the
first failure produces `rejected` with the reason, a journal entry
(`action: rejected`) and nothing leaves the machine:

1. thread known in state (`unknown thread`);
2. `session_id` belongs to the thread as driver or member (`foreign session`);
3. markers, `review_done` and `mr_note` carry a SHA equal to the head of the
   current round; for `review_done` the head is re-read via `Inspect`, falling
   back to the last known head when the provider is unreachable (`head moved to
   <sha>`);
4. marker order: `taken` once per thread and only from the driver;
   `review_start` after `taken`; `review_done` after `review_start` of the same
   round (`out of order`);
5. same `kind + text + sha` from the same session within 10 minutes
   (`duplicate`).

Delivery.

- Slack: label from `post_label` (default derived from `persona.agent`, e.g.
  `🤖 Atlas: `) is prepended by the daemon; a text that already starts with the
  label is rejected (`label in text`). Sending goes through `fetch_helper` in a
  `post` mode or, without a helper, through the built-in `slackapi` client with
  `chat.postMessage`.
  Markers are formatted by the daemon as the first line:
  `[TAKEN] <mr> / <sha>`, `[REVIEW START] <sha>`,
  `[REVIEW DONE] <sha> — blockers N, other M, findings in the MR: <url>; decision needed: <decision|none>`,
  with `(<agent>)` appended for committee members. Posting a marker records
  `progress` in the thread and journal immediately.
- MR: `glab mr note <iid> -m <text>` or `gh pr comment <n> --body <text>` via
  the daemon's commander, text prefixed with `(<agent>, <short sha>)`.

Receipt: `status` (`posted | rejected | pending`), `permalink`, `ts`, `reason`.
Journal: one entry per delivery with `kind`, `agent`, `sha`, `round`. Delivery
failure: three attempts, then dead letter of kind `outbox` with an alert and a
`pending` receipt; `replay` returns it to delivery.

## 3. Review runs

Thread record gains `run` and `runs_history`:

```
run: { round, head_sha, base_sha, started_at, opened_by: handoff|head_changed,
       results: { <agent>: { sha, blockers, others, decision, findings, at } },
       joint: { sha, at, permalink } }
```

- Round 1 opens with the window. `[HEAD CHANGED]` from a person, or a head
  moved detected by `Inspect` during continuation, opens round N+1: results and
  joint reset, the previous run is appended to `runs_history`. The continuation
  text says "round N, head <sha>, previous results void".
- A member `review_done` whose SHA equals the round head lands in
  `results[agent]`. With an old SHA it is still posted, suffixed by the daemon
  with `(round N is stale, head <sha>)`, not counted, journaled as
  `stale_progress`.
- When results exist for every member whose window is alive (dead members are
  dropped from the wait and shown as "did not submit"), the daemon sends the driver a
  continuation rendered from `results.tmpl`: per agent blockers, others,
  decision and the list of that agent's MR notes of this round (text + sha from
  the writer journal).
- Joint `review_done` is accepted only from the driver session, only when the
  current round's results are complete, only with the round head SHA; it fills
  `joint`, sets the thread phase `review_done` and closes the outcome.
  Otherwise `rejected: waiting for <agents>`.
- Single review uses the same run with one participant: its `review_done` is
  the joint one.
- Free-text markers arriving as Slack notifications keep today's SHA fence for
  windows started before this version; after the writer they are duplicates.

## 4. Prompts, errors, watchdog, tests

Prompts (`ru` and `en`, `initial`, `continuation`, new `results`): protocol
rewritten to `handoffd post` commands; bridge and `glab mr note`
forbidden explicitly; `rejected: head moved` means a new round (fetch,
`review-start` again); committee member submits `--review-done` and never the
joint verdict; the driver waits for the results continuation. Re-reading the
thread through Slack MCP to find `(codex)` is removed.

Errors: malformed outbox record moves to `outbox/rejected/` with a `malformed`
receipt; delivery failures as in section 2; `Inspect` failure falls back to the
known head. Watchdog: results complete without a joint verdict longer than
`blocked_alert_minutes` raises "round N is collected, no final verdict".

Tests: `internal/outbox` (write, read, receipt, malformed, atomicity);
pipeline (each rejection rule, duplicate, label, Slack and MR delivery through
fakes, dead letter + replay, pending receipt); runs (new round by
`[HEAD CHANGED]` and by head move at continuation, late result of a previous
round, collection and driver continuation, joint verdict refused before
completion, dead member, single review through the same run, old free-text
marker as duplicate); `cmd` (`post` without the daemon prints `pending`, exit
2); `agent` (new allow/deny lines); `slackapi` (`chat.postMessage` against
httptest). The Python helper's `post` mode is checked by hand, as the rest of
the script is today.
