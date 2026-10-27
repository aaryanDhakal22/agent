# Releasing the agent

The agent ships as a single static **`linux/arm64`** binary (no Docker). CI
(`.github/workflows/build.yml`) builds it and publishes GitHub Releases; a Pi
gets it through the **signed manifest + verified bootstrap** (`deploy/pi/` in
the ops repo at the root, runbook `deploy/pi/RECOVERY.md`) — never by hand.

**There is one way to tag a release: `deploy/release.sh agent` (ops repo
root), after the owner merged the PR into `master`.** Never `git tag vX.Y.Z`
by hand, and never tag a `staging` commit — the tag must sit on the merge
commit `origin/master` points at.

## TL;DR — what triggers what

| You do | CI does | Result |
|---|---|---|
| `git push` to the **`staging` branch** | verify (go vet + test) → build arm64 → update the **rolling `staging-latest` prerelease** | The latest staging binary at `staging-latest`, overwritten each push |
| `git push` to **any other branch** | verify + build only (artifact kept 7 days) | — |
| PR `staging` → `master`, merged by the owner (merge commit) | verify + build on `master` | — |
| **`deploy/release.sh agent`** (ops repo root) | tags `origin/master` `vX.Y.Z` from its `VERSION` → the tag's CI re-labels the tested build of the same tree + VERSION as the **immutable release** `vX.Y.Z` | A permanent prod binary at release `vX.Y.Z`, published to the prod Pi manifest |

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

1. **Bump `VERSION` in the work itself** (on `staging`, e.g. `2.3.5` →
   `2.3.6`, plain `X.Y.Z`, no `v`) and describe the release in the commit. CI
   stamps the binary with this VERSION.
2. **PR `staging` → `master`.** The owner reviews and merges it (merge commit).
3. **From the ops repo root:**

   ```bash
   deploy/release.sh agent          # asks before tagging; --yes skips the question
   ```

   It reads `VERSION` on `origin/master`, refuses if `vX.Y.Z` already exists
   on another commit (bump the version instead — never move a released tag),
   tags `origin/master` and pushes the tag, waits for the tag's CI run, then
   publishes the signed **prod** Pi manifest for `vX.Y.Z`
   (`deploy/pi/publish.sh --env prod --agent vX.Y.Z …`). Run it again after
   an interruption: a tag already on `origin/master` is resumed, not re-made.
4. The prod Pi installs it by itself right after the next Close batch
   (auto-update, §2.8 of `contract/agent.md`), or from the admin site / its web
   UI. New or swapped SD cards install the published release on first boot.

Confirm what's running: `curl -s localhost:8080/api/version` →
`{service, version, commit}` (or `GET /api/update/status` →
`current_version`, `latest_version`, `update_available`); fleet-wide:
the console's `--do fleet prd`.

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
  `git describe` diagnostics without changing anything. This repo has no
  tagging tasks any more (`task release`, `version-bump`, `version-tag` and
  `version-init` were removed in 2.3.6).
