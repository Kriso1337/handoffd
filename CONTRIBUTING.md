# Contributing to handoffd

Contributions are welcome through GitHub issues and pull requests.

## Development

Requirements:

- Docker or a compatible container runtime
- Git

Run the project checks:

```sh
docker build --target test .
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test -race ./...
docker run --rm -v "$PWD":/src -w /src golangci/golangci-lint:v2.5.0 golangci-lint run ./...
```

Every behavior change should include tests for the main scenario and relevant
edge cases. Keep changes focused and avoid unrelated refactoring.

## Pull requests

1. Open an issue first for substantial behavior or architecture changes.
2. Create a focused branch from `main`.
3. Add or update tests and documentation.
4. Run all checks listed above.
5. Explain the user-visible behavior and compatibility impact in the PR.

Do not commit Slack tokens, cookies, message content, local configuration,
runtime state, logs, internal hostnames or real workspace identifiers. Use
synthetic values in tests and examples.

By submitting a contribution, you agree that it is licensed under Apache-2.0.
