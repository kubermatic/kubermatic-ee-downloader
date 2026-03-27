# Kubermatic EE Downloader

A CLI tool to download Kubermatic Enterprise Edition binaries from OCI registries.

## Overview

`kubermatic-ee-downloader` pulls enterprise tool binaries (such as the conformance tester) from their OCI registries and saves them locally.

### Authentication

Credentials are resolved in the following order:

1. **CLI flags** — `--username` and `--password`
2. **Docker config** — `~/.docker/config.json` (e.g. after `docker login`)
3. **Interactive prompt** — if credentials are still missing, the tool asks on stdin

## Installation

### From Source

```bash
make build
# Binary is written to bin/kubermatic-ee-downloader
```

### Go Install

```bash
go install k8c.io/kubermatic-ee-downloader/cmd@latest
```

## Usage

### List Available Tools

```bash
kubermatic-ee-downloader list
```

### Download a Tool

```bash
# Interactive credentials prompt
kubermatic-ee-downloader get conformance-tester

# With explicit credentials and options
kubermatic-ee-downloader get conformance-tester \
  --username user \
  --password pass \
  --tag v1.2.0 \
  --output /usr/local/bin
```

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--username` | `-u` | Registry username |
| `--password` | `-p` | Registry password |
| `--tag` | `-t` | Artifact tag (default: `latest`) |
| `--registry` | `-r` | Override OCI registry |
| `--output` | `-o` | Output directory (default: `.`) |
| `--verbose` | `-v` | Enable verbose logging |

## Development

```bash
make fmt          # Format code
make vet          # Run go vet
make lint         # Run golangci-lint
make test         # Run tests
make verify-all   # Run all verification checks
```

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
