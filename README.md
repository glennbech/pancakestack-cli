# pancakestack

Stack your astro photos in the cloud. Your laptop stays cool.

Point it at a folder of light frames, kick off a stack, and get a `result.fit`
back when it's done. You take the photos; pancakestack does the heavy lifting.

## Install

### macOS

```bash
# Homebrew (auto-detects Apple Silicon vs Intel)
brew install --cask glennbech/tap/pancakestack
xattr -cr "$(brew --prefix)/Caskroom/pancakestack"   # clear Gatekeeper quarantine (unsigned binary)

# — or plain curl (no quarantine dance):
curl -sSL "https://github.com/glennbech/pancakestack-cli/releases/latest/download/pancakestack_darwin_$(uname -m | sed 's/x86_64/amd64/').tar.gz" | tar xz pancakestack && sudo mv pancakestack /usr/local/bin/
```

The release binary isn't codesigned+notarized yet, so the Homebrew cask
needs the `xattr` step. The `curl` path pipes through `tar` and never
sets the quarantine attribute, so it works out of the box.

### Linux (amd64 or arm64)

Homebrew casks are macOS-only, so on Linux use curl:

```bash
curl -sSL "https://github.com/glennbech/pancakestack-cli/releases/latest/download/pancakestack_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" | tar xz pancakestack && sudo mv pancakestack /usr/local/bin/
```

### Windows (PowerShell, amd64)

```powershell
$dir = "$env:LOCALAPPDATA\Programs\pancakestack"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Invoke-WebRequest -Uri "https://github.com/glennbech/pancakestack-cli/releases/latest/download/pancakestack_windows_amd64.zip" -OutFile "$dir\pancakestack.zip"
Expand-Archive -Force "$dir\pancakestack.zip" -DestinationPath $dir
```

Then add `%LOCALAPPDATA%\Programs\pancakestack` to your `PATH` (one-time,
via *Settings → System → About → Advanced system settings → Environment
Variables*), open a fresh terminal, and `pancakestack --version` should work.

### From source

```bash
go install github.com/glennbech/pancakestack-cli/cmd/pancakestack@latest
```

## Quickstart

```bash
# One-time: sign in with Google
pancakestack login

# Upload a night's worth of light frames — one S3 object per FITS
pancakestack upload m81 ~/astro/m81/lights/*.fit

# Kick off a stack (Seestar default). The backend picks the compute tier
# from frame count + drizzle — no instance flag to worry about.
pancakestack stack m81

# Follow the job's log
pancakestack jobs                    # find the job id
pancakestack logs <job-id> --follow  # tail it

# Download the finished stack (and every input file) back to disk
pancakestack download m81
```

You'll get an email when the stack finishes.

## Configuration

`pancakestack` reads its backend URL from (in order):
1. `--url` flag
2. `PANCAKESTACK_URL` env var
3. Compiled-in default (prod)

Auth tokens live in `~/.config/pancakestack/credentials.json` (mode 0600).

## Commands

| Command | Purpose |
|---|---|
| `pancakestack login` | Open browser, sign in with Google, save tokens |
| `pancakestack whoami` | Print the signed-in user |
| `pancakestack logout` | Delete stored tokens |
| `pancakestack upload <collection> <path>...` | Upload FITS files (or a tar/zip archive) to a collection |
| `pancakestack download <collection> [dest]` | Download every file in a collection, resumable |
| `pancakestack stack <collection>` | Kick off a stack on a previously-uploaded collection |
| `pancakestack jobs [job-id]` | List your jobs, or show details for one |
| `pancakestack cancel <job-id>...` | Cancel a running job and terminate its instance |
| `pancakestack logs <job-id>` | Print CloudWatch logs (`--follow`, `--tail N`, `--since 5m`) |
| `pancakestack metrics <job-id>` | Show CPU / memory / net / EBS time series for a job |
| `pancakestack ask "<query>"` | Ask the RAG (Siril manual + curated astrophoto docs) |
| `pancakestack seestar …` | Talk to a ZWO Seestar on your LAN — see [Seestar integration](#seestar-integration) below |

Run `pancakestack <command> --help` for the full flag list on any of them.

### Upload modes

`pancakestack upload` picks the mode from the paths you hand it:

```bash
# Multi-file — one S3 object per FITS, per-file dedup and resume:
pancakestack upload m81 ~/astro/m81/lights/*.fit

# Single archive — you already have a tar/zip:
pancakestack upload m81 lights.tar.zst

# Directory — tars locally, uploads once:
pancakestack upload m81 ~/astro/m81/lights/
```

Multi-file is the recommended path (per-file retry, resumable across
crashes; files already on S3 skip on re-run). Pass `--archived` on the
multi-file path to create the collection in cold storage (counts as 70%
against your storage quota; stacking paused for 30 days).

### Stack subsets

`pancakestack stack <collection>` runs against every frame in the
collection by default. Pass `--files` to narrow to an allowlist — the
same list the webapp's Filter Frames flow sends:

```bash
pancakestack stack m81 --files light_001.fit,light_002.fit,light_003.fit
```

## Seestar integration

Power-user only. `pancakestack seestar` talks to a ZWO Seestar smart
telescope over your local network — enumerate observation folders,
list frames, and continuously stream new FITS into a collection as the
scope captures them.

**You need your own RSA key.** Firmware 7.18 and later require a
challenge-response handshake on the JSON-RPC port, and the private key
is not distributed with this CLI. The extraction has to happen on your
Android phone against your own copy of the ZWO Seestar app — see
[astronomyk/seestarpy](https://github.com/astronomyk/seestarpy) for the
walkthrough and tooling.

Once you have the PEM, drop it in one of these locations (searched in
order — first hit wins):

1. Whatever `SEESTAR_KEY_PATH` points at (explicit override)
2. `~/.config/pancakestack/seestar.pem` (this CLI's config dir — recommended)
3. `./seestar.pem` (current working directory — handy for one-off scripts)
4. `~/.seestarpy/seestar.pem` (seestarpy compat — if you already extracted
   the PEM for that tool, you don't have to move it)

Both the file itself and the containing directory should be `chmod 700`
/ `600` — the key is per-device and there's no revocation flow.

**Prerequisite: station mode.** The scope has to be joined to your
home Wi-Fi (not broadcasting its own hotspot), so your laptop can reach
both it and the internet at the same time.

### Verbs

```bash
# UDP-broadcast for scopes on the local subnet
pancakestack seestar discover

# List every observation folder under MyWorks/ on the scope
pancakestack seestar ls

# List every FITS inside one folder
pancakestack seestar ls "M 81_sub"

# Continuously upload every new FITS from an observation folder into a
# pancakestack collection as the scope captures them. State lives in
# ~/.config/pancakestack/seestar-sync.json so a restart never re-uploads.
# On the FIRST sync of a folder pick either --from-now (skip whatever's
# already on the scope, only upload frames captured afterwards) or
# --backfill (upload everything that's already there too):
pancakestack seestar sync "M 81_sub" m81-tonight --from-now

# Auto-kick a stack once 50 frames have landed:
pancakestack seestar sync "M 81_sub" m81-tonight --from-now \
  --stack-when 50 --stack-script seestar-advanced
```

State is keyed by `(folder, filename)` — not by scope serial — so
swapping scopes (S30 → S50p, firmware reflash, etc.) keeps
deduplication working. A pre-2.0 state file with serial-in-key
entries is migrated in place on first load.

On sync start the CLI also reconciles local state against the target
collection's current contents: any file already in the collection is
recorded as already-uploaded, so a QA-delete on the webapp never
causes the file to churn back in on the next sync. Files deleted
BEFORE this feature existed will re-upload once, then stay deleted
after the next QA pass.

While `seestar sync` is running, the webapp's collection detail page
shows a green **"Live import in progress"** banner — heartbeated by
the CLI every few seconds and cleared automatically when the process
exits.

Multiple scopes on the LAN? Pass `--ip <address>` to any subcommand to
skip discovery.

## License

MIT — see [LICENSE](LICENSE).
