# handoffd

[![CI](https://github.com/Kriso1337/handoffd/actions/workflows/ci.yml/badge.svg)](https://github.com/Kriso1337/handoffd/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

`handoffd` helps developers notice and respond to actionable team messages on
time. It watches the local Slack Desktop log, retrieves message context, filters
noise and opens an isolated Claude Code or Codex session when a message needs
attention.

The default workflow is response assistance, not autonomous code review. MR/PR
review automation is disabled by default and must be explicitly enabled with
`review_enabled: true`.

## What it does

- Detects notifications and activity in remembered threads without continuously
  polling every Slack conversation.
- Filters bots, system events and short acknowledgements in code.
- Uses a configurable lightweight model for ambiguous messages.
- Opens or resumes a dedicated agent window with the message, permalink and
  thread context.
- Delivers later replies to the same live session.
- Persists admitted messages, decisions, waiting prompts and failed deliveries
  so restarts do not silently lose work.
- Supports tmux, WezTerm, herdr and cmux.
- Can optionally prepare exact GitLab MR or GitHub PR worktrees for read-only
  review workflows.

```text
Slack Desktop log
      │
      ▼
notification parser ── bot/system/ack ──► ignored
      │
      ▼
deterministic routing ── direct request ─► agent session
      │
      ▼
triage model ─────────── actionable ─────► agent session
      │
      └───────────────── noise ──────────► silent

Optional review mode (`review_enabled: true`)
      │
      └─ explicit review signal + MR/PR ─► pinned read-only worktree
```

## Safety model

The watcher is designed around explicit boundaries:

- Generic response sessions investigate and prepare an answer locally. They do
  not post to Slack, modify repositories, merge or deploy without an explicit
  instruction from the owner.
- Review sessions only exist when `review_enabled` is true.
- Review worktrees are pinned to a provider-confirmed head and base revision.
- Review agents receive a read-only permission profile. Push, merge, approval
  and direct provider comments are denied.
- Outbound Slack messages and MR/PR notes go through one daemon-owned writer
  that validates the thread, session, revision and marker order.
- Ambiguous delivery failures are never retried automatically when a duplicate
  message could be created.

## Requirements

- macOS or Linux
- Slack Desktop
- Docker or OrbStack for builds and tests
- At least one coding agent: Claude Code or Codex
- At least one supported terminal backend: tmux, WezTerm, herdr or cmux
- Slack access through either:
  - the built-in Web API client and a token; or
  - an external helper implementing the documented JSON contract
- `git` and, for optional review mode, `glab` and/or `gh`

## Installation

```sh
git clone https://github.com/Kriso1337/handoffd.git
cd handoffd
make install
```

`make install` builds and tests the project in Docker, installs the binary into
`~/.local/bin`, runs the setup wizard and installs a user service:

- launchd on macOS
- systemd user service on Linux

Useful commands:

```sh
make configure
make status
make logs
make restart
make uninstall
```

## Configuration

The setup wizard writes `~/.config/handoffd/config.json`. A minimal example:

```json
{
  "self_user_id": "U0000000001",
  "slack_workspace": "acme",
  "channel": "C0000000001",
  "channel_name": "agent-handoffs",
  "mention_words": ["atlas"],
  "triage_words": ["atlas", "alex"],
  "macro_phrases": ["atlas, help", "atlas help"],
  "stop_phrases": ["atlas, stop", "atlas stop"],
  "review_enabled": false,
  "persona": {
    "owner": "Alex",
    "owner_full": "Alex Smith",
    "agent": "Atlas",
    "approvers": "Alex",
    "capabilities": "Jira, GitLab, Grafana",
    "thread_tool": "Slack MCP"
  },
  "prompts_language": "en",
  "terminal": "tmux",
  "tmux_session": "main",
  "projects_dir": "/Users/developer/projects",
  "gitlab_hosts": ["gitlab.example.com"],
  "github_hosts": ["github.com"]
}
```

The public distribution supports English prompts only. Identity, trigger
phrases, models, tools, repository locations and provider hosts are local
configuration rather than source-code defaults.

Message text is Unicode and language-agnostic. Configure mention, macro, stop,
acknowledgement and review word lists for the languages used by your team.

### Slack access

With an empty `fetch_helper`, the built-in client reads a token from
`slack_token_file` or `HANDOFFD_SLACK_TOKEN`. Browser `xoxc-` tokens also need
the `d` cookie through `slack_cookie_file` or `HANDOFFD_SLACK_COOKIE`.

Read access needs the relevant history and metadata scopes:

```text
channels:history groups:history im:history mpim:history
channels:read groups:read im:read mpim:read users:read
```

Add `chat:write` only when the daemon is configured to publish responses.

An external helper receives one of these forms:

```text
<helper> <channel> <message_ts> [thread_ts]
<helper> --replies <channel> <thread_ts> <since_ts>
<helper> --post <channel> <thread_ts> <text>
```

The message command returns one JSON object with author, text, channel and
thread context. The replies command returns `{"replies": [...]}` and the post
command returns `{"ts": "..."}`. `HANDOFFD_SELF_USER_ID` is available in the
helper environment.

### Optional review mode

Review automation is opt-in:

```json
{
  "review_enabled": true,
  "review_markers": ["[handoff]", "[review"],
  "review_head_markers": ["[head changed]", "[review start]"],
  "review_words": ["review", "re-review", "take a look at the mr", "take a look at the pr"],
  "gitlab_hosts": ["gitlab.example.com"],
  "github_hosts": ["github.com"]
}
```

When disabled, review-looking messages are handled as ordinary notifications:
they can be surfaced to the owner, but no review worktree, reviewer permission
profile, review committee or review protocol is started.

When enabled, an explicit review signal must include an MR/PR link. The daemon
verifies the provider host, resolves the exact head and base, creates a detached
worktree and opens the agent with read-only tools. Review output is submitted
through `handoffd post` so the daemon can reject stale revisions and invalid
marker order.

## CLI

```text
handoffd status
handoffd status --short
handoffd status --json
handoffd decisions --since 24h
handoffd label <ts|permalink> good|bad [note]
handoffd eval [--since 720h]
handoffd replay
handoffd prune -n
handoffd setup-agent [claude|codex]
handoffd init
handoffd terminal-check
handoffd fetch <channel> <ts> [thread_ts]
handoffd link --thread <root_ts> --kind help "context"
```

Review-only commands require `review_enabled: true`:

```text
handoffd link --thread <root_ts> --kind review "<MR or PR URL>"
handoffd post --thread <root_ts> --marker taken --sha <sha>
handoffd post --thread <root_ts> --marker review-start --sha <sha>
handoffd post --thread <root_ts> --mr-note --sha <sha> "finding"
handoffd post --thread <root_ts> --review-done --sha <sha> --blockers N --others M
```

## Local data and privacy

The daemon processes names, Slack user IDs, message text, thread excerpts,
task links and local repository paths. Depending on configuration, that data is
sent to the selected coding agent and stored locally in state, journal, inbox,
outbox, dead-letter and prompt files.

Never publish local configuration, tokens, cookies, Slack logs or the runtime
state directory. The repository ignores their common filenames, and GitHub
secret scanning with push protection is enabled.

Default locations:

```text
~/.config/handoffd/config.json
~/.config/handoffd/prompts/*.tmpl
~/.config/handoffd/slack_token
~/.config/handoffd/slack_cookie
~/.local/state/handoffd/state.json
~/.local/state/handoffd/decisions.jsonl
~/.local/state/handoffd/inbox.jsonl
~/.local/state/handoffd/outbox/
~/.local/state/handoffd/waiting/
~/.local/state/handoffd/deadletter.jsonl
```

## Development

```sh
docker build --target test .
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test -race ./...
docker run --rm -v "$PWD":/src -w /src golangci/golangci-lint:v2.5.0 golangci-lint run ./...
```

GitHub Actions runs formatting, vet, lint and race-enabled tests for every pull
request. See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines and
[SECURITY.md](SECURITY.md) for private vulnerability reporting.

## License

Licensed under [Apache-2.0](LICENSE).
