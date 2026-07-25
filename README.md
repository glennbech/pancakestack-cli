# pancakestack

Cloud astrophotography stacking on AWS EC2 spot instances. Command-line client.

Upload a tar of your light frames, kick off a stack, get a `result.fit` back.
Auth is Google via AWS Cognito.

## Install

Pick one:

```bash
# Homebrew (macOS + Linux)
brew install glennbech/tap/pancakestack

# Or download a release binary
curl -sSL https://github.com/glennbech/pancakestack-cli/releases/latest/download/pancakestack_$(uname -s)_$(uname -m).tar.gz | tar xz -C /usr/local/bin pancakestack

# Or if you have Go
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
