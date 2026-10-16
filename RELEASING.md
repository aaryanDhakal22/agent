# Releasing the agent

The agent ships as a single static **`linux/arm64`** binary (no Docker). CI
(`.github/workflows/build.yml`) builds it and publishes GitHub Releases; you
download the binary to the Pi and run it. There are exactly two tracks — read
this before you touch `git tag` by hand.

> **Interactive path:** the console `Ship` flow (`console/`, key `c`) now does
> add → commit → push → version-bump+tag natively and guardrailed, so you rarely
> hand-run `task release` anymore (it stays valid). And note the **current**
> staging Pi was planted **manually** (see `../STAGING.md`) — it does *not* yet
> use the release-download flow described below; that's the target state.

## TL;DR — what triggers what

| You do | CI does | Result |
|---|---|---|
| `git push` to the **`staging` branch** | `verify` (go vet + test) → build arm64 → update the **rolling `staging-latest` prerelease** | Latest staging binary at the `staging-latest` release, overwritten each push |
| `git push` to **any other branch** | `verify` + build only (artifact kept 7 days) | — |
| **`task release -- patch\|minor\|major`** (from `agent/`) | bump `VERSION`, commit, tag `vX.Y.Z`, push → `verify` → build → cut an **immutable release** for `vX.Y.Z` | A permanent, addressable prod binary at release `vX.Y.Z` |

**Never `git tag vX.Y.Z` by hand.** Always use `task release`, so `VERSION` and
the tag stay in lockstep and CI stamps the binary with the matching version.

## Staging — nothing to do

Every push to the `staging` branch publishes to the single rolling
`staging-latest` prerelease (named so it can't clash with the branch). To use it on the Pi:

```bash
gh release download staging-latest -p 'agent-linux-arm64*'   # binary + .sha256
sha256sum -c agent-linux-arm64.sha256
doppler run -- ./agent-linux-arm64                    # stop with: pkill -f agent-linux-arm64
```

The `staging-latest` release is updated to the newest build each push, so re-downloading
always gives you the latest. (This is also what the console's stg lifecycle SSHes
onto the Pi to run.)

## Production — cut a version

From the **`agent/`** directory, one command:

```bash
task release -- patch     # 0.1.0 -> 0.1.1   (bug fix)
task release -- minor     # 0.1.0 -> 0.2.0   (feature)
task release -- major     # 0.1.0 -> 1.0.0   (breaking)
# `task release` with no arg defaults to patch.
```

What it does (via `../scripts/version.sh bump`), in order:

1. Refuses if the working tree is dirty (anything other than `VERSION`) — commit
   or stash first.
2. Bumps `agent/VERSION`, commits `chore(release): vX.Y.Z`, creates the annotated
   tag `vX.Y.Z`.
3. `git push origin HEAD` + `git push origin --tags`.
4. CI's production job (`refs/tags/v*`) runs verify → builds the arm64 binary
   with `-X main.version=X.Y.Z` → creates the **immutable** GitHub Release
   `vX.Y.Z` with `agent-linux-arm64` + `.sha256`.

Then on each Pi:

```bash
gh release download v1.2.3 -p 'agent-linux-arm64*'
sha256sum -c agent-linux-arm64.sha256
pkill -f agent-linux-arm64 ; doppler run -- ./agent-linux-arm64
```

Confirm what's running: `curl -s localhost:8080/api/version` → `{version, commit}`.

## Guardrails (why `version.sh` may refuse)

- **Dirty tree** → "commit or stash them before bumping." Only `VERSION` may be
  uncommitted.
- **Tag already exists** (`vX.Y.Z`) → pick a different bump, or delete the tag
  first. Don't force-retag a released version.
- **No upstream branch** → it won't push a tag pointing at a commit no one else
  has. Set the branch's upstream first.
- Tags are **annotated** (`git describe` / Go `runtime/debug` see them as real
  releases). Don't create lightweight tags by hand.

## Rollback

Releases are immutable and version-addressable, so rolling back is just
downloading an older one:

```bash
gh release list                                   # find the good version
gh release download v1.2.2 -p 'agent-linux-arm64*'
sha256sum -c agent-linux-arm64.sha256
pkill -f agent-linux-arm64 ; doppler run -- ./agent-linux-arm64
```

(Future: a CD job will roll `prod-latest` across the fleet; today it's a manual
`gh release download` per Pi.)

## Notes

- The binary is CGO-free and self-contained (embedded migrations, embedded web
  UI). It needs only its env (Doppler: `MAIN_SERVER_URL`, `AGENT_DB_PATH`, OTEL,
  Pushover) and a writable dir for its SQLite file — no runtime assets.
- `task version-current` / `task version-describe` show the current version +
  `git describe` diagnostics without changing anything.
