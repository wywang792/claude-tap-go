# Contributing

Thanks for helping improve `claude-tap-go`.

## Development Setup

```bash
go test ./...
go build ./cmd/claude-tap
```

Please keep changes focused and include tests for behavior changes when practical.

## Pull Requests

- Describe the problem and the user-visible behavior change.
- Mention which client mode was tested, for example `claude` reverse proxy or `gemini` forward proxy.
- Do not include `.traces/`, logs, local caches, built binaries or secrets.

## Sensitive Data

Trace files may contain prompts, responses, tool inputs and local file paths. Before sharing a trace in an issue or pull request, redact private or sensitive content.
