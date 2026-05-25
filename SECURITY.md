# Security Policy

## Reporting a Vulnerability

Please do not open a public issue for vulnerabilities that expose prompts, credentials, local files or upstream API traffic.

Until a dedicated security contact is configured, email the repository owner or use GitHub private vulnerability reporting if it is enabled for the repository.

## Data Handling

`claude-tap-go` is a local debugging tool. It records request and response metadata and bodies to `.traces/`. Treat those files as sensitive. Authorization-like headers are redacted, but prompts, model output, tool inputs and paths may still contain private data.
