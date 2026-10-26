# Releasing the agent

The agent ships as a single static **`linux/arm64`** binary (no Docker). CI
(`.github/workflows/build.yml`) builds it and publishes GitHub Releases; a Pi
gets it through the **signed manifest + verified bootstrap** (`deploy/pi/` in
the ops repo at the root, runbook `deploy/pi/RECOVERY.md`) — never by hand.
There are exactly two tracks — read this before you touch `git tag` by hand.

> **Interactive path:** the console `Ship` flow (`console/`, key `c`) now does
> add → commit → push → version-bump+tag natively and guardrailed, so you rarely
> hand-run `task release` anymore (it stays valid). The staging Pi runs the same
> Pi layout as a restaurant Pi since 2026-10-16 (see `../STAGING.md`).

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
`staging-latest` prerelease (named so it can't clash with the branch). To put it
on the staging Pi, publish the signed staging manifest (from the repo root),
then update the Pi:

```bash
deploy/pi/publish.sh --env staging --agent staging-latest --main-url http://192.168.1.221:1323
```

Then either the Pi's **Update now** (web UI › Settings › Account, only right
after Close batch), an admin-site update (RPC `agent.update`, may be forced —
it waits for an idle store), or `/usr/local/lib/quicc/bootstrap.sh` on the Pi
(`../STAGING.md`). The root updater verifies the manifest + SHA-256 before it
installs and keeps the previous release.

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

Then publish the prod manifest for that tag (prod accepts only immutable
`v*` releases):

```bash
deploy/pi/publish.sh --env prod --agent vX.Y.Z --main-url https://main.quiccpos.com
```

and update each Pi from the admin site or its web UI (right after Close batch;
§2.8 of `contract/agent.md` has the gates). New / swapped SD cards install the
published release on first boot.

Confirm what's running: `curl -s localhost:8080/api/version` →
`{service, version, commit}` (or `GET /api/update/status` →
`current_version`, `latest_version`, `update_available`).

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

Each Pi keeps the previous release next to the current one
(`/opt/quicc-agent/{current,previous}`): **Roll back** (web UI › Settings ›
Account, or the admin site's RPC `agent.rollback`; only with no open orders)
swaps them and restarts; a second rollback toggles back. The root updater also
rolls back by itself when the new release fails its health check. To pin the
fleet to an older version, re-publish the manifest with that tag
(`deploy/pi/publish.sh --env prod --agent v1.2.2 …`) — releases are immutable
and version-addressable (`gh release list`).

## Notes

- The binary is CGO-free and self-contained (embedded migrations, embedded web
  UI). It needs only its env (`MAIN_SERVER_URL`, `APP_ENV`, `AGENT_DB_PATH`, …
  — Doppler in dev, `/etc/quicc-agent.env` on a Pi; OTEL + Pushover arrive
  from main at the manager-login link) and a writable dir for its SQLite file —
  no runtime assets.
- `task version-current` / `task version-describe` show the current version +
  `git describe` diagnostics without changing anything.
