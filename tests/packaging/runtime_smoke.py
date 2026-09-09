"""Exercise the shipped entrypoint and HTTP API with synthetic secrets only."""
from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path


def docker(*args: str) -> str:
    return subprocess.run(["docker", *args], check=True, capture_output=True, text=True).stdout.strip()


def main() -> None:
    runtime_image, test_image, redis_name = sys.argv[1:]
    name = redis_name + "-runtime"
    fixture = redis_name + "-fixture"
    volume = redis_name + "-data"
    token = "123456:synthetic-packaging-token"
    adapter = json.dumps({"job_url": "http://127.0.0.1:18080/job/fixture/", "username": "fixture", "api_token": "synthetic-jenkins-secret"})
    port = docker("port", redis_name, "8080/tcp").rsplit(":", 1)[1]
    base = "http://127.0.0.1:" + port

    def request(path: str, payload=None, cookie=None):
        headers = {"Content-Type": "application/json"}
        if cookie:
            headers["Cookie"] = cookie
        req = urllib.request.Request(base + path, data=None if payload is None else json.dumps(payload).encode(), headers=headers)
        try:
            response = urllib.request.urlopen(req, timeout=3)
        except urllib.error.HTTPError as error:
            response = error
        return response.status, response.read(), response.headers

    def healthy():
        for _ in range(60):
            try:
                if request("/api/health")[:2] == (200, b'{"status":"ok"}\n'):
                    return
            except (OSError, TimeoutError):
                pass
            time.sleep(1)
        raise AssertionError("packaged Gateway did not become healthy")

    try:
        docker("run", "-d", "--name", fixture, "--network", "container:" + redis_name,
               "--env", "GOMAXPROCS=2", test_image, "go", "run", "./tests/packaging/fixture")
        for _ in range(60):
            try:
                docker("exec", redis_name, "wget", "-qO-", "http://127.0.0.1:18080/")
                break
            except subprocess.CalledProcessError:
                time.sleep(1)
        else:
            raise AssertionError("synthetic Telegram fixture did not start")
        docker("volume", "create", volume)
        docker("create", "--name", name, "--network", "container:" + redis_name,
               "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=32m",
               "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "SETGID",
               "--cap-add", "SETUID", "--security-opt", "no-new-privileges:true",
               "--volume", volume + ":/data", runtime_image,
               "-addr", ":8080", "-db", "/data/botdata.db", "-redis-addr", "127.0.0.1:6379",
               "-tg-api", "http://127.0.0.1:18080")
        with tempfile.TemporaryDirectory() as directory:
            secrets = Path(directory) / "secrets"
            secrets.mkdir()
            for filename, contents in (("telegram_bot_token", token), ("adapter_config", adapter)):
                path = secrets / filename
                path.write_text(contents + "\n")
                path.chmod(0o600)
            docker("cp", str(secrets), name + ":/run/secrets")
        docker("start", name)
        healthy()
        assert request("/api/auth/me")[0] == 401
        assert request("/api/gateway/v1/ops/health")[0] == 401
        assert request("/api/auth/login", {"username": "admin", "password": "wrong"})[0] == 401
        status, _, headers = request("/api/auth/login", {"username": "admin", "password": "admin"})
        assert status == 200
        cookie = headers["Set-Cookie"].split(";", 1)[0]
        assert request("/api/auth/me", cookie=cookie)[0] == 200
        # A persisted session observes SQLite continuity through process restart.
        docker("exec", name, "sh", "-ec", r'''
uid=$(sed -n 's/^Uid:[[:space:]]*\([0-9]*\).*/\1/p' /proc/1/status)
[ "$uid" != 0 ]
[ "$(sed -n 's/^CapEff:[[:space:]]*//p' /proc/1/status)" = 0000000000000000 ]
for secret in telegram_bot_token adapter_config; do
    [ "$(stat -c %a /tmp/$secret)" = 600 ]
    [ "$(stat -c %u /tmp/$secret)" = "$uid" ]
    cmp /run/secrets/$secret /tmp/$secret
done
''')
        key_before = docker("exec", name, "sha256sum", "/data/.gateway-key")
        docker("restart", name)
        healthy()
        assert request("/api/auth/me", cookie=cookie)[0] == 200
        assert key_before == docker("exec", name, "sha256sum", "/data/.gateway-key")
        logs = docker("logs", name)
        assert token not in logs and "synthetic-jenkins-secret" not in logs
        print("PASS packaged secrets, privilege drop, health, authentication and SQLite/key restart")
    finally:
        for container in (name, fixture):
            subprocess.run(["docker", "rm", "-fv", container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
