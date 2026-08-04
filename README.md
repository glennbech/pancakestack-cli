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

# Upload a set of light frames
pancakestack upload m81 ~/astro/m81/lights

# Kick off a stack (Seestar default)
pancakestack stack m81

# ...or with drizzle 2× on a bigger instance
pancakestack stack m81 --script seestar-drizzle --instance r7g.8xlarge
```

You'll get an email when the stack finishes.

## Configuration

`pancakestack` reads its backend URL from (in order):
1. `--url` flag
2. `PANCAKESTACK_URL` env var
3. Compiled-in default

Auth tokens live in `~/.config/pancakestack/credentials.json` (mode 0600).

## Commands

| Command | Purpose |
|---|---|
| `pancakestack login` | Open browser, sign in with Google, save tokens |
| `pancakestack whoami` | Print the signed-in user |
| `pancakestack logout` | Delete stored tokens |
| `pancakestack upload <collection> <path>` | Tar the path and upload as a collection |
| `pancakestack stack <collection> [--script X] [--param k=v] [--instance T]` | Stack a previously-uploaded collection |

## License

MIT — see [LICENSE](LICENSE).
