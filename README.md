# EduKod Local Companion

[![CI](https://github.com/edukod-cz/edukod-companion/actions/workflows/ci.yml/badge.svg)](https://github.com/edukod-cz/edukod-companion/actions/workflows/ci.yml)
[![CodeQL](https://github.com/edukod-cz/edukod-companion/actions/workflows/codeql.yml/badge.svg)](https://github.com/edukod-cz/edukod-companion/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/edukod-cz/edukod-companion)](https://github.com/edukod-cz/edukod-companion/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

The Local Companion lets one school-managed Linux workstation provide an
OpenAI-compatible model (Ollama by default) to its EduKod installation. It
opens one outbound `wss://` connection to EduKod and talks only to a loopback
model API. It does not expose a listener, execute EduKod tools, or receive MCP
credentials.

The same binary works with every EduKod school. The school URL and a revocable
device credential are supplied during pairing; no school hostname, fleet
credential, or model selection is compiled into a release.

## Public repository scope

This repository contains only the Linux Companion client, its tests,
documentation, and release packaging. It does not contain any EduKod server,
administration/control-plane, deployment, tenant configuration, or credential
source. The enrollment and WSS route names documented here are the public
client protocol required for interoperability; their server implementation is
not distributed with the Companion.

## Requirements

- Linux on amd64 or arm64.
- An EduKod school administrator or fleet operator who can create a one-time
  pairing code.
- A loopback OpenAI-compatible API. Ollama at
  `http://127.0.0.1:11434/v1` is the default.
- Outbound HTTPS and WSS access to the paired school's EduKod origin.

## Install and pair

Use the exact install command shown in **EduKod Admin > AI > Local Companion**
or the EduKod School Console. Release artifacts contain a static
`edukod-companion` binary for Linux amd64
and arm64, this guide, and the systemd user unit. Copy the minisign public key
from the exact install command in **EduKod Admin > AI** or the School Console;
those interfaces receive it from the fleet operator's protected configuration.
Do not trust a key learned only from the same GitHub release as the package.
`COMPANION_MINISIGN.pub` is published so you can compare it with that
independently obtained key. Verify both `SHA256SUMS` and its signature before
installing a release.

```bash
minisign -Vm SHA256SUMS -x SHA256SUMS.minisig -P '<key copied from EduKod Admin or School Console>'
sha256sum -c SHA256SUMS
```

On Debian or Ubuntu, install the matching verified package:

```bash
sudo apt install ./edukod-companion_<version>_linux_amd64.deb
systemctl --user daemon-reload
```

For the static archive, install the binary and user unit manually:

```bash
sudo install -m 0755 edukod-companion /usr/bin/edukod-companion
install -Dm0644 edukod-companion.service \
  "$HOME/.config/systemd/user/edukod-companion.service"
systemctl --user daemon-reload
```

Install and start Ollama, then download a model. The default local API is
`http://127.0.0.1:11434/v1`.

```bash
ollama pull qwen3
```

In EduKod, open **Admin > AI > Local Companion** and create a one-time pairing
code. Run the exact command displayed there; it has this shape:

```bash
edukod-companion pair \
  --school https://your-school.edukod.cz \
  --code ONE-TIME-CODE \
  --name "School AI workstation"
edukod-companion doctor
systemctl --user enable --now edukod-companion.service
edukod-companion status
```

`pair` uses Linux Secret Service when available and otherwise writes the
credential to its private configuration file. On a headless account whose
keyring will not unlock at boot, select the deliberate fallback:

```bash
edukod-companion pair ... --credential-store file
```

For a dedicated headless account, an administrator may enable user lingering
so the user service starts at boot without an interactive login:

```bash
sudo loginctl enable-linger "$USER"
```

The browser never contacts `localhost`. Pairing is completed directly by the
native daemon over HTTPS, and inference uses its persistent outbound WSS
connection.

## Commands

- `pair` exchanges a single-use code for a revocable device credential.
- `doctor` verifies private credential storage and the local `/v1/models` API.
- `run` maintains the WSS connection and relays bounded requests.
- `status` shows pairing and the last connection result without revealing a
  credential.
- `models` prints the local OpenAI-compatible model list.
- `unpair` revokes the device and removes local credentials. Use
  `--local-only` only after separately revoking the device in EduKod.

All commands accept `--config-dir` for testing or nonstandard installations.
`pair --local-url` accepts only an `http://` or `https://` URL whose path is
exactly `/v1` and whose hostname resolves entirely to loopback addresses.

## Security boundary

The daemon accepts only:

- `GET /v1/models`
- `POST /v1/responses`
- `POST /v1/chat/completions`

It forwards no caller-supplied URL or header. DNS is checked again for every
local connection; any non-loopback address is rejected. Requests are limited
to 8 MiB, responses to 16 MiB, concurrency to four, and the pending queue to
16. Deadlines and cancellation propagate to the local model. Requests are not
replayed after a connection failure. Streaming requests are rejected in
protocol v1; EduKod retains and replays the full non-streaming tool history.

On Linux desktops the credential is stored through Secret Service when
`secret-tool` is available. Headless installations fall back to a mode-`0600`
file in a mode-`0700` configuration directory. Logs and status files contain
connection metadata only, never prompts, model responses, or credentials.

See [SECURITY.md](SECURITY.md) for supported versions and private vulnerability
reporting. Please do not publish suspected security issues in a public issue.

## Development

Go 1.23 or newer is recommended. The project has no third-party Go module
dependencies.

```bash
make check
make build
./bin/edukod-companion version
```

## Release builds

```bash
make check
make dist VERSION=0.1.0
make sign VERSION=0.1.0 MINISIGN_SECRET_KEY=/secure/release.key
```

Maintainers publish releases from signed `v<version>` tags. `make dist` and
`make deb` create Debian/Ubuntu packages for amd64 and arm64 in
addition to the static archives. Release automation must supply the protected
minisign key; unsigned artifacts must not be published as production releases.

The GitHub release workflow requires:

- repository secret `COMPANION_MINISIGN_SECRET_KEY`, containing the unencrypted
  minisign secret key used only by the release runner;
- repository variable `COMPANION_MINISIGN_PUBLIC_KEY`, containing the matching
  single-line `RW...` public key.

Pushing a tag such as `v0.1.0` builds, signs, attests, and publishes the release.
The secret key must also have an offline backup; GitHub secrets are write-only
and are not a backup mechanism.

Before expanding a canary, run the opt-in compatibility smoke test with a model
that is already pulled on the Linux host. It sends one synthetic request through
Responses and one through Chat Completions; it never downloads a model itself.

```bash
EDUKOD_OLLAMA_SMOKE_MODEL=qwen3 go test ./internal/localapi \
  -run TestRealOllamaOpenAICompatibility -v
```

## Wire protocol

Pairing uses `POST /api/ai/companion/v1/enroll`. The daemon then connects to
`wss://<school>/api/ai/companion/v1/ws` with its device bearer credential. It
sends a versioned `hello`, accepts multiplexed `request`, `cancel`, and `ping`
JSON messages, and returns one `response` per accepted request. Request IDs are
unique while active. A bounded reconnect loop uses jitter and never replays an
ambiguous request.
