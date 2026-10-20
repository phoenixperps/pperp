# pperp

The Phoenix Perpetuals command line (`pperp`) and the hosted MCP server (`pperp-mcpd`), built from one operation table
(`internal/ops`, generated from the API's spec: `go generate ./...`), so every API operation is one command and one
MCP tool with the same name and arguments. Both are clients of the public API only.

## pperp

```
pperp login --sandbox            # sign in to the sandbox in your browser (pperp login: live)
pperp accounts                   # your accounts
pperp use SANDBOX-00042-001      # the default account of this profile
pperp quote BTC-USD              # bid, ask and last
pperp buy BTC-USD 0.01 --tp 2% --sl 1%
pperp sell ETH-USD '$5000' --limit 3600 --dry-run
pperp positions                  # orders, fills, risk, history equity|daily
pperp close BTC-USD --percent 50
pperp flatten                    # every order canceled, every position closed (asks for the account's name)
pperp stream account             # every event, live; stream prices|book|trades|candles|stats MARKET...
pperp list-orders --status history --all-pages -o csv
pperp api GET /v1/limits         # any path of the profile's host
pperp mcp --sandbox              # the MCP tools over stdio, for an assistant on this computer
```

- One command per API operation (`pperp --help` lists them): path parameters positional, the account from
  `--account` (else the profile's default, else your only one), query and body fields as flags, nested bodies through
  `--data @file.json` or `--set exits.stop_loss.price=61000`.
- Profiles `live` and `sandbox` (and your own: `pperp login --profile NAME --env sandbox`). Settings in
  `config.toml` (`%APPDATA%\pperp` on Windows, `~/.config/pperp` elsewhere; `PPERP_CONFIG_DIR` to move it); the
  credential in the system keychain only (`--store file` to keep it in a file readable by you only).
  `PHOENIXPERPS_API_KEY` is used instead for one call; a live key is never kept by pperp.
- Live trades are shown with the API's what-if (price, fee, margin) and asked about first (`confirm_trades`, off for
  the sandbox); with no terminal they need `--yes`. Flatten and cancel-all ask for the account's name.
- Output: a table on a terminal, JSON when piped; `-o json|jsonl|csv|table`, `--jq`. Decimals exactly as sent.
- Exit codes: 0 done, 1 failed or declined, 2 usage, 3 not signed in or not allowed, 4 not found, 5 conflict,
  6 refused by a rule, 7 rate limited, 8 server, 9 network or unknown outcome (repeat with the `--idempotency-key`
  printed, after checking).

## pperp-mcpd

The MCP server at https://mcp.phoenixperpsfunding.com (POST / live, POST /sandbox), Streamable HTTP, stateless. Live
takes an assistant's OAuth sign-in only; the sandbox also takes a sandbox key. Reads by default; trading only on a
grant that allows it, previewed first when the grant asks for it. `deploy/deploy-mcpd.sh` builds and deploys it.

## Building

```
go generate ./...   # spec/openapi.json -> internal/ops/ops_gen.go
go test ./...
go build -o dist/ ./cmd/...
```

The Go SDK comes from the SDK repository beside this one (`replace ../sdk/go` in go.mod) until it is published.

## Releasing pperp

Releases are signed on the computer that keeps the release key (minisign format: Ed25519 over BLAKE2b-512), never in
CI or on a server, and served as static files from the docs host (`/install.sh`, `/install.ps1`, `/downloads/cli/`).
`pperp upgrade` installs a version only when latest.json and its SHA256SUMS are signed by a key built into it
(`internal/release/keys`), the archive matches, and the new binary runs.

```
go run ./tools/release keygen                    # once: the secret key outside OneDrive; commit internal/release/keys/pperp.pub
go run ./tools/release build -version 1.0.0      # tests, the table, the six archives and SHA256SUMS in dist/v1.0.0
go run ./tools/release sign -version 1.0.0       # asks the key's password; signs SHA256SUMS and latest.json
go run ./tools/release publish -version 1.0.0 -host root@<docs box>
```

A new key is rolled out with `keygen -next -key <new path>`: releases built from then on trust both, and the next
releases are signed with the new one.
