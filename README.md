# pperp

The Phoenix Perpetuals command line: your accounts, orders, positions and live data from a terminal, and the API's
tools for an AI assistant on your computer (`pperp mcp`). Full guide: https://docs.phoenixperpsfunding.com/cli

## Install

```
curl -fsSL https://docs.phoenixperpsfunding.com/install.sh | sh          # macOS and Linux
irm https://docs.phoenixperpsfunding.com/install.ps1 | iex                # Windows (PowerShell)
go install github.com/phoenixperps/pperp/cmd/pperp@latest               # with Go: the same release, built here
```

The installers check the download against the release's SHA256SUMS, and SHA256SUMS against the release key pinned in
the installer itself (openssl, LibreSSL or .NET; minisign if you have it). Without any of them nothing is installed,
unless `PPERP_INSECURE=1` (the checksum only, and the installer says so). The release keys:

- minisign `RWQ1BrEPEK9erW/k12JUZKOaMmm2/Iu0WH2Z6zBnn/1g/9qU+Vg/K9S2` (key id AD5EAF100FB10635)
- ECDSA P-256, SHA-256 of the public key `4741c9bebdb60c70cb63206dd00b04b8fdf7aa0e16ed57df4186508eb1ef289f`

`pperp upgrade` installs a newer pperp only when it is signed by a key built into yours.

## First steps

```
pperp login --sandbox            # sign in to the sandbox in your browser (pperp login: live)
pperp accounts                   # your accounts
pperp use SANDBOX-00042-001      # the default account of this profile
pperp quote BTC-USD              # bid, ask and last
pperp buy BTC-USD 0.01 --tp 2% --sl 1%
pperp sell ETH-USD '$500' --dry-run   # what it would cost, nothing sent
pperp positions                  # also: orders, fills, risk, history equity|daily
pperp close BTC-USD --percent 50
pperp flatten                    # every order canceled, every position closed
pperp stream account             # every event, live; stream prices|book|trades|candles|stats MARKET...
pperp doctor                     # checks this computer: settings, keychain, sign-in, the API, the clock
```

- One command per API operation (`pperp --help` lists them): path parameters positional, the account from
  `--account` (else the profile's default, else your only one), query and body fields as flags, nested bodies through
  `--data @file.json` or `--set exits.stop_loss.price=61000`. `pperp api GET /v1/limits` calls any path.
- Profiles `live` and `sandbox` (and your own: `pperp login --profile NAME --env sandbox`). Settings in `config.toml`
  (`%APPDATA%\pperp` on Windows, `~/.config/pperp` elsewhere; `PPERP_CONFIG_DIR` to move it), readable by you only;
  the credential in the system keychain (`--store file` keeps it in a file beside the settings instead).
  `PHOENIXPERPS_API_KEY` is used instead for one call; a live key is never kept by pperp.
- Asked before it runs: a live trade, shown with its price, fee and margin (`confirm_trades`, off for the sandbox);
  flatten, cancel-all and a new sandbox account, by typing the account's name. With no terminal, `--yes`.
- Output: a table on a terminal, JSON when piped; `-o json|jsonl|csv|table`, `--jq`. Decimals exactly as sent.
- Exit codes: 0 done, 1 failed or declined, 2 usage, 3 not signed in or not allowed, 4 not found, 5 conflict,
  6 refused by a rule, 7 rate limited, 8 server, 9 network or unknown outcome (repeat with the `--idempotency-key`
  printed, after checking).

## AI assistants

`pperp mcp --sandbox` serves the API's tools over stdio to an assistant on this computer, signed in as the profile.
Web assistants connect to the hosted server instead, https://mcp.phoenixperpsfunding.com (sign-in with OAuth): see
https://docs.phoenixperpsfunding.com/mcp

## Security

See [SECURITY.md](SECURITY.md): report a vulnerability to support@phoenixperpsfunding.com, never in a public issue.
