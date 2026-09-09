from __future__ import annotations

import subprocess
import unittest
from pathlib import Path


VERIFIED_UPSTREAM_REVISION = "4819842ff90b4675d60b79eaf577a73d6740b6d8"
FIXED_FORK_REVISION = "cfae7f856f9864a401d6220e8ca42dd8b70a19b0"


class TestTelegramGatewayContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.repo_root = Path(__file__).resolve().parents[2]
        cls.gateway_root = cls.repo_root / "ee/telegram_gateway"

    def test_fork_is_pinned_to_the_fixed_downstream_revision(self) -> None:
        result = subprocess.run(
            ["git", "merge-base", "--is-ancestor", FIXED_FORK_REVISION, "HEAD"],
            cwd=self.repo_root, capture_output=True,
        )
        self.assertEqual(result.returncode, 0)

        upstream = (self.gateway_root / "UPSTREAM.md").read_text(encoding="utf-8")
        self.assertIn(VERIFIED_UPSTREAM_REVISION, upstream)
        self.assertIn(FIXED_FORK_REVISION, upstream)
        self.assertIn("Apache-2.0", upstream)
        self.assertIn("Local modification boundary", upstream)

    def test_image_build_is_source_pinned_and_runs_unprivileged(self) -> None:
        dockerfile = (self.repo_root / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn("golang:1.26-alpine@sha256:", dockerfile)
        self.assertIn("alpine:3.21@sha256:", dockerfile)
        self.assertIn("COPY . ./", dockerfile)
        self.assertIn(
            'org.opencontainers.image.revision="${COMMIT}"',
            dockerfile,
        )
        self.assertIn("su-exec", dockerfile)
        self.assertNotIn("USER telegram-gateway", dockerfile)
        self.assertIn("HEALTHCHECK", dockerfile)
        self.assertIn(
            'ENTRYPOINT ["/usr/local/bin/telegram-gateway-entrypoint"]', dockerfile
        )

    def test_dev_compose_has_runtime_and_credential_boundaries(self) -> None:
        compose = (self.gateway_root / "compose.yaml").read_text(encoding="utf-8")
        self.assertIn("127.0.0.1:18081:8080", compose)
        self.assertIn("telegram_gateway_data:/data", compose)
        self.assertIn("file: ./secrets/telegram_bot_token", compose)
        self.assertIn("target: telegram_bot_token", compose)
        self.assertIn("read_only: true", compose)
        self.assertIn("no-new-privileges:true", compose)
        self.assertIn("cap_drop:", compose)
        self.assertIn("cap_add:", compose)
        self.assertIn("- DAC_OVERRIDE", compose)
        self.assertIn("- SETGID", compose)
        self.assertIn("- SETUID", compose)
        self.assertNotIn("- CHOWN", compose)
        self.assertIn("restart: unless-stopped", compose)
        self.assertIn("healthcheck:", compose)
        self.assertNotIn("TELEGRAM_BOT_TOKEN:", compose)
        self.assertNotIn("TELEGRAM_CHAT_ID", compose)
        self.assertNotIn("/var/run/docker.sock", compose)

    def test_entrypoint_passes_the_secret_file_without_exporting_the_token(self) -> None:
        entrypoint = (self.gateway_root / "entrypoint.sh").read_text(encoding="utf-8")
        self.assertIn("umask 077", entrypoint)
        self.assertIn('cat > "$1"', entrypoint)
        self.assertIn(
            'exec su-exec telegram-gateway:telegram-gateway',
            entrypoint,
        )
        self.assertNotIn("TELEGRAM_BOT_TOKEN", entrypoint)
        self.assertNotIn("telegram_token=", entrypoint)

    def test_smoke_entrypoint_is_serial_and_covers_preserved_interfaces(self) -> None:
        smoke = self.gateway_root / "smoke.sh"
        listed = subprocess.run(
            [str(smoke), "--list"],
            cwd=self.repo_root,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.splitlines()
        self.assertEqual(
            listed,
            ["management", "polling", "push-proxy", "bot-api-proxy"],
        )

        source = smoke.read_text(encoding="utf-8")
        self.assertIn("flock -n", source)
        self.assertIn("status --porcelain", source)
        self.assertIn("go test -p 1", source)
        self.assertIn("docker run --detach", source)
        self.assertIn("--log-driver none", source)
        self.assertIn("--cap-add DAC_OVERRIDE", source)
        self.assertIn("--cap-add SETGID", source)
        self.assertIn("--cap-add SETUID", source)
        self.assertIn("curl --fail --silent", source)
        self.assertIn("TestResolveTelegramToken", source)
        self.assertIn("TestE2E_Errors", source)
        self.assertIn("TestE2E_RestartWaitsForPriorPollingOwner", source)
        self.assertIn("PushProxyDeliversWithoutSensitiveLogs", source)
        self.assertIn("ProxyLogsRedactTokenAndChat", source)
        self.assertNotIn("set -x", source)
        self.assertNotIn("TELEGRAM_CHAT_ID", source)

    def test_dev_host_deploy_is_managed_and_credential_safe(self) -> None:
        deploy = self.gateway_root / "deploy_dev.sh"
        self.assertTrue(deploy.stat().st_mode & 0o111)

        source = deploy.read_text(encoding="utf-8")
        self.assertIn("WINDMILL_SSH_USER", source)
        self.assertIn("WINDMILL_SSH_IP", source)
        self.assertIn("/opt/telegram_gateway", source)
        self.assertIn("docker save", source)
        self.assertIn("docker load", source)
        self.assertIn("--no-build", source)
        self.assertIn("chmod 600", source)
        self.assertIn("stop telegram_gateway", source)
        self.assertIn("configured token owner", source)
        self.assertIn(".Config.Entrypoint", source)
        self.assertIn(".Mounts", source)
        self.assertIn("cmp -s", source)
        self.assertIn("cannot safely inspect local container token ownership", source)
        self.assertIn("cannot safely inspect remote container token ownership", source)
        self.assertIn("cannot list local containers for token ownership inspection", source)
        self.assertIn("cannot list remote containers for token ownership inspection", source)
        self.assertIn('[ "${inspect_status}" -eq 1 ] || return 2', source)
        self.assertIn("sleep 35", source)
        self.assertIn("polling ownership conflict", source)
        self.assertNotIn("set -x", source)
        self.assertNotIn("TELEGRAM_BOT_TOKEN", source)

    def test_docs_define_upgrade_regression_and_rollback_workflows(self) -> None:
        readme = (self.gateway_root / "README.md").read_text(encoding="utf-8")
        for heading in (
            "## DEV startup",
            "## Smoke validation",
            "## Upstream upgrade",
            "## Roll back to the fixed baseline",
            "## Polling ownership",
        ):
            self.assertIn(heading, readme)
        self.assertIn("build.sh", readme)
        self.assertIn("smoke.sh", readme)


if __name__ == "__main__":
    unittest.main()
