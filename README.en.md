# claude-tap-go

[中文](README.md)

`claude-tap-go` is a Go port / experimental implementation of [claude-tap](https://github.com/liaohch3/claude-tap), a local API trace proxy and viewer for AI coding CLIs.

I started this project because the original Python version did not work reliably for me on Windows due to compatibility issues. This Go version is primarily aimed at making the workflow usable in my Windows environment.

## Current Status

Please note the current testing scope:

- Developed and tested only on Windows.
- Tested only with Claude Code so far.
- Linux and macOS have not been tested.
- Configurations for Codex, Gemini, Kimi, OpenCode, Hermes, Cursor, Pi, Qoder and Antigravity exist in the code, but I have not verified them yet.
- If you run into issues on another platform or client, please open an Issue.

## Demo

The images below are adapted from the original `claude-tap` project. This Go port follows the same general trace/viewer idea, but details may differ.

<p align="center">
  <img src="docs/demo_zh.gif" alt="claude-tap trace viewer demo" width="100%">
</p>

<p align="center">
  <img src="docs/architecture.png" alt="claude-tap architecture overview" width="90%">
</p>

## Build

Requires Go 1.22 or newer.

```powershell
git clone https://github.com/wywang792/claude-tap-go.git
cd claude-tap-go
go build -o claude-tap.exe ./cmd/claude-tap
```

## Usage

Start Claude Code through the proxy:

```powershell
.\claude-tap.exe
```

Run only the proxy:

```powershell
.\claude-tap.exe --tap-no-launch --tap-port 18080
```

Disable the live viewer:

```powershell
.\claude-tap.exe --tap-no-live
```

Open the session dashboard:

```powershell
.\claude-tap.exe dashboard --tap-live-port 8989
```

Export a trace:

```powershell
.\claude-tap.exe export .traces/2026-05-22/trace_120000.jsonl -o report.md
```

## Data Notice

Trace files may contain prompts, model responses, tool arguments, local file paths, request bodies and response bodies. Treat `.traces/` as sensitive data and do not commit it to a public repository.

## Development

```powershell
go test ./...
go vet ./...
go build ./cmd/claude-tap
```

## License

MIT. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
