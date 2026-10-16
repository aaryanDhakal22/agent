# Deploying the agent on a Pi

`quicc-agent.service` runs the static `agent-linux-arm64` binary under
`doppler run` with `Restart=always` (see the comments in the unit for the
install steps). The tablet pairs to `<pi-ip>:8080`.

Without the Doppler CLI (the staging DietPi), replace `ExecStart` with
`/opt/quicc-agent/agent-linux-arm64` and add `EnvironmentFile=/etc/quicc-agent.env`
rendered from Doppler (`doppler secrets download --no-file --format env`) —
re-render it after a Doppler change.

Replacing the v1 agent (Docker + Postgres + nginx on :80): stop it with
`docker compose down` in its directory (keep it on disk for rollback), then
`systemctl enable --now quicc-agent`. Never run both — they would print every
order twice once both are linked.

Releases: see `../RELEASING.md` (branch push → rolling `staging` prerelease;
`task release` → immutable `vX.Y.Z`).
