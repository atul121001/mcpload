# Homebrew formula

`mcpload.rb.tmpl` is the formula template. On every release tag, the `homebrew` job in [release.yml](../../.github/workflows/release.yml) fills in the version and the SHA-256 of each macOS/Linux archive from the release's `checksums.txt` and pushes the result to `Formula/mcpload.rb` in the tap repo [`atul121001/homebrew-tap`](https://github.com/atul121001/homebrew-tap). Users then install with:

```sh
brew install atul121001/tap/mcpload
```

The formula unpacks the release folder into Homebrew's `libexec` and links only `mcpload` into `bin`.

## One-time setup (repo owner)

The job is skipped, with a notice in the run log, until both of these exist.

1. **Create the tap repo.** On GitHub, create a public repo named `homebrew-tap` under `atul121001`, with a README so it has a `main` branch. Homebrew maps `atul121001/tap` to `github.com/atul121001/homebrew-tap`. The job creates `Formula/` on its first run.
2. **Create a token that can push to it.** Go to GitHub, Settings, Developer settings, Personal access tokens, Fine-grained tokens, Generate new token:
   - Repository access: *Only select repositories* → `atul121001/homebrew-tap`
   - Permissions: *Contents: Read and write* (nothing else)
   - Choose an expiry and put a reminder in your calendar to renew it.
3. **Add it as a secret** in `atul121001/mcpload`: Settings, Secrets and variables, Actions, New repository secret, name `HOMEBREW_TAP_TOKEN`.

The next `v*` tag publishes the formula. To publish one for a release that already exists, re-run the `homebrew` job of that release's workflow run (re-runs use the current secrets), or render it locally (see below) and commit it to the tap by hand.

## Render locally

```sh
curl -fsSLO https://github.com/atul121001/mcpload/releases/download/v0.4.0/checksums.txt
node packaging/homebrew/render.mjs v0.4.0 checksums.txt mcpload.rb
ruby -c mcpload.rb    # syntax check
```

For a full install test on a Mac, put it in a throwaway local tap:

```sh
brew tap-new "$USER/local" --no-git
cp mcpload.rb "$(brew --repo "$USER/local")/Formula/"
brew install "$USER/local/mcpload" && brew test "$USER/local/mcpload"
brew uninstall mcpload && brew untap "$USER/local"
```
