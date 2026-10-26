"""Shared fixtures for agent/ pytest.

Two kinds of target:

  * `spawn_agent` — a THROWAWAY agent: the binary is built once per session,
    then each test gets its own process on a spare port with a temp SQLite DB,
    pointed at main (MAIN_BASE_URL). It starts blank (`unconfigured`), so the
    provisioning state machine can be driven end-to-end without touching the
    dev roster agents. A matching throwaway tenant + location is created in
    main (admin API) and deleted on teardown.
  * `agent_base_url` — an already-running agent (default the primary roster
    agent on :8080) for the real-printer tests.

Env:
    TARGET_ENV          dev (default). The throwaway agent is dev-only.
    MAIN_BASE_URL       default http://localhost:1323
    ADMIN_API_KEY       main's admin bearer (dev fallback: ../main/.doppler-dev-main)
    AGENT_BASE_URL      default http://localhost:${HTTP_PORT:-8080}
    AGENT_UI_PASSWORD   that agent's web-UI password (real-printer tests log in;
                        dev roster default `aaryan`)
    TEST_REAL_PRINTERS  set to 1 to include real_printer-marked tests
"""

from __future__ import annotations

import os
import secrets
import socket
import socketserver
import subprocess
import tempfile
import threading
import time
from contextlib import closing
from pathlib import Path
from typing import Iterator

import pytest
import requests

AGENT_DIR = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_DIR.parent
TARGET_ENV = os.getenv("TARGET_ENV", "dev")
HTTP_PORT = os.getenv("HTTP_PORT", "8080")
AGENT_BASE_URL = os.getenv("AGENT_BASE_URL", f"http://localhost:{HTTP_PORT}")
MAIN_BASE_URL = os.getenv("MAIN_BASE_URL", "http://localhost:1323").rstrip("/")
ADMIN_V1 = f"{MAIN_BASE_URL}/admin/v1"
STORE_V1 = f"{MAIN_BASE_URL}/store/v1"
REAL_PRINTERS = os.getenv("TEST_REAL_PRINTERS") == "1"


def _admin_key() -> str:
    if os.getenv("ADMIN_API_KEY"):
        return os.environ["ADMIN_API_KEY"]
    if TARGET_ENV != "dev":
        return ""
    try:
        token = (REPO_ROOT / "main/.doppler-dev-main").read_text().strip()
        out = subprocess.run(["doppler", "secrets", "get", "ADMIN_API_KEY", "--plain"],
                             env=_doppler_env(token), capture_output=True, text=True, timeout=20)
        return out.stdout.strip() if out.returncode == 0 else ""
    except (OSError, subprocess.SubprocessError):
        return ""


def _doppler_env(token: str) -> dict:
    """Env for a nested `doppler secrets get`: drop inherited DOPPLER_* (an outer
    `doppler run` exports DOPPLER_PROJECT/CONFIG, which would override the
    token's own project scope) and use only the given service token."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("DOPPLER_")}
    env["DOPPLER_TOKEN"] = token
    return env


ADMIN_API_KEY = _admin_key()


def pytest_collection_modifyitems(config, items):
    """Skip real_printer-marked tests unless TEST_REAL_PRINTERS=1."""
    if REAL_PRINTERS:
        return
    skip_real = pytest.mark.skip(reason="TEST_REAL_PRINTERS is not set to 1")
    for item in items:
        if "real_printer" in item.keywords:
            item.add_marker(skip_real)


def _free_port() -> int:
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_until(fn, *, timeout: float = 15.0, interval: float = 0.25, what: str = "condition"):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = fn()
            if last:
                return last
        except requests.RequestException as exc:
            last = exc
        time.sleep(interval)
    raise AssertionError(f"{what} not met within {timeout}s (last={last!r})")


# ---------------------------------------------------------------------------
# main admin + a throwaway store for the agent to link to
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def admin() -> requests.Session:
    if TARGET_ENV != "dev":
        pytest.skip("throwaway-agent tests are dev-only")
    if not ADMIN_API_KEY:
        pytest.skip("ADMIN_API_KEY unavailable (env or ../main/.doppler-dev-main)")
    try:
        requests.get(f"{MAIN_BASE_URL}/health", timeout=3).raise_for_status()
    except requests.RequestException as exc:
        pytest.skip(f"main not reachable at {MAIN_BASE_URL}: {exc}")
    s = requests.Session()
    s.headers.update({"Authorization": f"Bearer {ADMIN_API_KEY}", "Content-Type": "application/json"})
    return s


class MainStore:
    """Throwaway tenant + location in main; mint() returns a fresh device token."""

    def __init__(self, admin: requests.Session) -> None:
        self.admin = admin
        tag = secrets.token_hex(4)
        r = admin.post(f"{ADMIN_V1}/tenants", json={"slug": f"t-{tag}", "name": f"{tag} Agent Test"}, timeout=5)
        assert r.status_code == 201, r.text
        self.tenant_id = r.json()["id"]
        self.location_id = r.json()["location"]["id"]  # created with the tenant

    def mint(self) -> dict:
        r = self.admin.post(f"{ADMIN_V1}/devices", json={"location_id": self.location_id, "name": "pytest-agent"}, timeout=5)
        assert r.status_code == 201, r.text
        return {"token": r.json()["api_key"], "device_id": r.json()["device"]["id"]}

    def device(self, device_id: str) -> dict:
        return next(d for d in self.admin.get(f"{ADMIN_V1}/devices", timeout=5).json() if d["id"] == device_id)

    def delete(self) -> None:
        try:
            self.admin.delete(f"{ADMIN_V1}/tenants/{self.tenant_id}", timeout=10)
        except requests.RequestException:
            pass


@pytest.fixture
def main_store(admin) -> Iterator[MainStore]:
    s = MainStore(admin)
    yield s
    s.delete()


# ---------------------------------------------------------------------------
# Throwaway agent process
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def agent_binary(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("agent-bin") / "agent"
    r = subprocess.run(["go", "build", "-o", str(out), "./cmd/main.go"], cwd=AGENT_DIR,
                       env={**os.environ, "CGO_ENABLED": "0"}, capture_output=True, text=True, timeout=300)
    if r.returncode != 0:
        pytest.fail(f"agent build failed:\n{r.stderr}")
    return out


class AgentProc:
    def __init__(self, binary: Path, extra_env: dict | None = None) -> None:
        self.port = _free_port()
        self.base = f"http://127.0.0.1:{self.port}"
        self.dir = Path(tempfile.mkdtemp(prefix="quicc-agent-test-"))
        self.log = open(self.dir / "agent.log", "wb")
        env = {
            "PATH": os.environ.get("PATH", ""),
            "HOME": os.environ.get("HOME", str(self.dir)),
            "APP_ENV": "dev",
            "MAIN_SERVER_URL": MAIN_BASE_URL,
            "AGENT_DB_PATH": str(self.dir / "agent.db"),
            "HTTP_PORT": str(self.port),
            "LOG_LEVEL": "debug",
            "OTEL_EXPORTER_OTLP_ENDPOINT": "",  # no-op telemetry
            # LAN discovery off unless a test asks: the roster agent (or another
            # throwaway one) may already hold UDP 48080 on this host.
            "AGENT_DISCOVERY_PORT": "0",
            **(extra_env or {}),
        }
        self._binary, self._env = binary, env
        self.proc = subprocess.Popen([str(binary)], cwd=self.dir, env=env, stdout=self.log, stderr=subprocess.STDOUT)
        wait_until(lambda: requests.get(f"{self.base}/api/version", timeout=1).status_code == 200,
                   timeout=20, what="throwaway agent to serve")
        self.http = requests.Session()  # keeps the web-UI session cookie

    def restart(self) -> None:
        """Stop and start the same agent (same DB + port); the web session
        survives (persisted signing secret)."""
        self.proc.terminate()
        self.proc.wait(timeout=10)
        self.proc = subprocess.Popen([str(self._binary)], cwd=self.dir, env=self._env, stdout=self.log, stderr=subprocess.STDOUT)
        wait_until(lambda: requests.get(f"{self.base}/api/version", timeout=1).status_code == 200,
                   timeout=20, what="restarted agent to serve")

    def session(self) -> dict:
        return requests.get(f"{self.base}/api/session", timeout=5).json()

    def state(self, client: requests.Session | None = None) -> str:
        return (client or requests).get(f"{self.base}/api/session", timeout=5).json()["state"]

    def log_tail(self, n: int = 40) -> str:
        try:
            return "\n".join((self.dir / "agent.log").read_text(errors="replace").splitlines()[-n:])
        except OSError:
            return ""

    def stop(self) -> None:
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self.log.close()


@pytest.fixture
def spawn_agent(agent_binary):
    procs: list[AgentProc] = []

    def _spawn(**env: str) -> AgentProc:
        p = AgentProc(agent_binary, env)
        procs.append(p)
        return p

    yield _spawn
    for p in procs:
        p.stop()


@pytest.fixture
def blank_agent(spawn_agent) -> AgentProc:
    """One unconfigured throwaway agent."""
    return spawn_agent()


# ---------------------------------------------------------------------------
# Running-agent target (real-printer tests)
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def http() -> requests.Session:
    s = requests.Session()
    s.headers.update({"Content-Type": "application/json"})
    return s


@pytest.fixture(scope="session")
def agent_base_url() -> str:
    return AGENT_BASE_URL


@pytest.fixture(scope="session")
def agent_ready(http: requests.Session, agent_base_url: str) -> None:
    try:
        if http.get(f"{agent_base_url}/api/version", timeout=3).status_code == 200:
            return
    except requests.RequestException as exc:
        pytest.skip(f"agent not reachable at {agent_base_url}: {exc}")
    pytest.skip(f"agent not healthy at {agent_base_url}")


# ---------------------------------------------------------------------------
# Fake network printers + a provisioned agent with a printer roster
# ---------------------------------------------------------------------------


class FakePrinter:
    """A raw-print (port-9100-style) listener that keeps each job's bytes."""

    def __init__(self) -> None:
        self.jobs: list[bytes] = []
        outer = self

        class Handler(socketserver.BaseRequestHandler):
            def handle(self) -> None:
                chunks = []
                self.request.settimeout(5)
                try:
                    while data := self.request.recv(65536):
                        chunks.append(data)
                except (socket.timeout, OSError):
                    pass
                if chunks:  # detect probes connect + close with no payload
                    outer.jobs.append(b"".join(chunks))

        socketserver.ThreadingTCPServer.allow_reuse_address = True
        self.server = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.addr = f"127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def text(self) -> str:
        return b"\n----JOB----\n".join(self.jobs).decode("latin-1")

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def fakes():
    made: dict[str, FakePrinter] = {}
    yield made
    for f in made.values():
        try:
            f.stop()
        except OSError:
            pass


def manager_session(admin: requests.Session, store: MainStore) -> requests.Session:
    """A /app/store manager session for a throwaway store."""
    user, pw = f"mgr{secrets.token_hex(3)}", "pw-mgr"
    r = admin.patch(f"{ADMIN_V1}/tenants/{store.tenant_id}/credentials", json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    s = requests.Session()
    assert s.post(f"{STORE_V1}/session", json={"username": user, "password": pw}, timeout=5).status_code == 200
    return s


def provision(agent: AgentProc, store: MainStore, password: str = "pw") -> None:
    """Link a throwaway agent to the store and log its web-UI session in."""
    token = store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    r = agent.http.post(f"{agent.base}/api/setup/password", json={"password": password, "confirm": password}, timeout=10)
    assert r.status_code == 200, r.text
    # 2.2.1 auto-assign (default on) would pool ready deliveries by itself;
    # the floor/pool tests build their pools by hand.
    r = agent.http.put(f"{agent.base}/api/settings/deliveries", json={"auto_assign": False}, timeout=10)
    assert r.status_code == 200, r.text


@pytest.fixture(scope="session")
def agent_session(agent_base_url: str, agent_ready: None) -> requests.Session:
    """A logged-in web-UI session on the running agent (floor routes need it)."""
    pw = os.getenv("AGENT_UI_PASSWORD", "aaryan" if TARGET_ENV == "dev" else "")
    if not pw:
        pytest.skip("AGENT_UI_PASSWORD not set")
    s = requests.Session()
    r = s.post(f"{agent_base_url}/api/login", json={"password": pw}, timeout=5)
    if r.status_code != 200:
        pytest.skip(f"can't log in to the agent at {agent_base_url} ({r.status_code}) — set AGENT_UI_PASSWORD")
    return s
