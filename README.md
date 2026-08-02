# pancakestack

Stack your astro photos in the cloud. Your laptop stays cool.

Point it at a folder of light frames, kick off a stack, and get a `result.fit`
back when it's done. You take the photos; pancakestack does the heavy lifting.

## Install

Pick one:

```bash
# Homebrew (macOS + Linux)
brew install --cask glennbech/tap/pancakestack

# Or download a release binary (macOS / Linux, amd64 or arm64)
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -sSL "https://github.com/glennbech/pancakestack-cli/releases/latest/download/pancakestack_${os}_${arch}.tar.gz" \
  | tar xz pancakestack && sudo mv pancakestack /usr/local/bin/

# Or if you have Go
go install github.com/glennbech/pancakestack-cli/cmd/pancakestack@latest
```

### macOS: unsigned binary

The release binary isn't (yet) codesigned + notarized, so Gatekeeper will
block it on first run — you'll see a "cannot be opened" dialog. After a
`brew install --cask`, clear the quarantine bit:

```bash
xattr -cr "$(brew --prefix)/Caskroom/pancakestack"
```

The `curl` install above is unaffected — piping to `tar` doesn't set the
quarantine attribute.

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
