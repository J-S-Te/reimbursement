#!/usr/bin/env python3
"""真实临时文件与外部命令替身验证报销图准入，不连接业务环境。"""

import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import unittest

import test_ci_deployment as deployment_tests
from test_ci_deployment import IMAGE, ROOT


class ComposeAdmissionTests(unittest.TestCase):
    # 用途：复用隔离部署文件夹具，不执行或复制业务部署逻辑。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def setUp(self):
        fixture = deployment_tests.RemoteDeploymentTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        self.fixture = fixture
        self.deploy = fixture.deploy
        self.environment = dict(os.environ)
        self.environment["PATH"] = str(fixture.tools) + os.pathsep + self.environment["PATH"]
        self.environment["FAKE_DOCKER_TRACE"] = str(self.deploy / "docker-trace")
        self.helper = ROOT / "scripts/reimbursement-compose-admission.py"

    # 用途：执行真实准入命令并捕获脱敏结论。
    # 参数：self为实例，overrides为外部命令替身的有界故障设置。
    # 返回值：真实子进程结果。
    def invoke(self, **overrides):
        return subprocess.run(["python3", "-B", str(self.helper), str(self.deploy), IMAGE],
                              text=True, capture_output=True,
                              env={**self.environment, **overrides}, timeout=15)

    # 用途：验证只做固定加法、备份有效且不会启动或迁移报销业务。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_additive_graph_has_verified_private_backup_and_no_business_start(self):
        before_env = {name: (self.deploy / name).read_bytes() for name in (".env", ".release.env")}
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        response = json.loads(result.stdout)
        self.assertEqual(response["status"], "ADMITTED")
        backup = self.deploy / response["backup"]
        self.assertEqual((backup / "docker-compose.yml").read_text(), self.fixture.original)
        expected = hashlib.sha256(self.fixture.original.encode()).hexdigest()
        self.assertEqual((backup / "docker-compose.yml.sha256").read_text(), expected + "  docker-compose.yml\n")
        for path in backup.iterdir():
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        current = (self.deploy / "docker-compose.yml").read_text()
        self.assertEqual(current.replace(self.fixture.block, "").replace(self.fixture.volumes, ""), self.fixture.original)
        for name, raw in before_env.items():
            self.assertEqual((self.deploy / name).read_bytes(), raw)
        calls = [json.loads(line) for line in (self.deploy / "docker-trace").read_text().splitlines()]
        self.assertTrue(calls)
        self.assertTrue(all(call[-2:] == ["config", "--quiet"] for call in calls))
        self.assertFalse((self.deploy / "called.txt").exists())
        self.assertFalse((self.deploy / "runtime/.control-plane-reload-required").exists())

    # 用途：验证完整服务图再次准入不改文件、不重复备份或重载。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_complete_graph_is_idempotent_without_dist(self):
        self.assertEqual(self.invoke().returncode, 0)
        current = (self.deploy / "docker-compose.yml").read_bytes()
        before = sorted((self.deploy / "backups").iterdir())
        reloads = (self.deploy / "reload-trace").read_text()
        (self.deploy / "docker-compose.yml.dist").unlink()
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "UNCHANGED")
        self.assertEqual((self.deploy / "docker-compose.yml").read_bytes(), current)
        self.assertEqual(sorted((self.deploy / "backups").iterdir()), before)
        self.assertEqual((self.deploy / "reload-trace").read_text(), reloads)

    # 用途：验证部分存在、重复与异常来源不能覆盖当前服务图。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_conflicting_or_malformed_graph_is_rejected_before_writes(self):
        source = (self.deploy / "docker-compose.yml.dist").read_text()
        conditions = {
            "partial-service": (self.fixture.original.replace("networks:\n", "  reimbursement-api:\n    image: custom\nnetworks:\n"), source),
            "partial-volume": (self.fixture.original + "  reimbursement-mysql-data:\n", source),
            "duplicate-service": (self.fixture.original.replace("networks:\n", "  platform-api:\n    image: duplicate\nnetworks:\n"), source),
            "unexpected-source-service": (self.fixture.original, source.replace("  # END SUBSYSTEM:", "  unrelated-service:\n    image: x\n  # END SUBSYSTEM:")),
            "duplicate-source-marker": (self.fixture.original, source + self.fixture.block),
            "quoted-extra-service": (self.fixture.original, source.replace("  # END SUBSYSTEM:", '  "unexpected-service":\n    image: x\n  # END SUBSYSTEM:')),
            "inline-external-volume": (self.fixture.original, source.replace("  reimbursement-mysql-data:\n", "  reimbursement-mysql-data: {external: true}\n")),
            "oversized-source": (self.fixture.original, "#" * (4 * 1024 * 1024 + 1)),
        }
        for name, (current, dist) in conditions.items():
            with self.subTest(condition=name):
                (self.deploy / "docker-compose.yml").write_text(current)
                (self.deploy / "docker-compose.yml.dist").write_text(dist)
                result = self.invoke()
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((self.deploy / "docker-compose.yml").read_text(), current)
                self.assertFalse((self.deploy / "reload-trace").exists())
                self.assertFalse((self.deploy / "backups").exists())

    # 用途：验证共享锁与既有恢复标记阻断操作，不覆盖其他任务状态。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_shared_lock_and_pending_markers_block_admission(self):
        runtime = self.deploy / "runtime"
        runtime.mkdir()
        with (runtime / ".deploy.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.invoke()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("并发", result.stderr)
        for name in (".assets-install-transaction", ".control-plane-reload-required", ".runtime-approval-publish"):
            marker = runtime / name
            marker.write_text("OTHER_TASK=preserved\n")
            result = self.invoke()
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(marker.read_text(), "OTHER_TASK=preserved\n")
            marker.unlink()
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)

    # 用途：验证文件和事务目录符号链接被拒绝，不写穿外部目标。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_symlink_input_and_lock_are_rejected(self):
        for name in ("docker-compose.yml", "docker-compose.yml.dist", ".env", "bin/provisioner-config-refresh.sh"):
            with self.subTest(path=name):
                path = self.deploy / name
                saved = path.with_name(path.name + ".saved")
                path.rename(saved)
                path.symlink_to(saved)
                try:
                    self.assertNotEqual(self.invoke().returncode, 0)
                finally:
                    path.unlink()
                    saved.rename(path)
        runtime = self.deploy / "runtime"
        runtime.mkdir(exist_ok=True)
        external = self.deploy / "outside-lock"
        external.write_text("untouched")
        (runtime / ".deploy.lock").unlink(missing_ok=True)
        (runtime / ".deploy.lock").symlink_to(external)
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertEqual(external.read_text(), "untouched")

    # 用途：验证Compose校验故障不会改图或泄漏外部错误中的敏感值。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_config_failure_has_no_write_and_suppresses_sensitive_error(self):
        docker = self.fixture.tools / "docker"
        docker.write_text("#!/bin/sh\necho 'SYNTHETIC_SECRET_MUST_NOT_ESCAPE' >&2\nexit 9\n")
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("SYNTHETIC_SECRET", result.stdout + result.stderr)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)
        self.assertFalse((self.deploy / "backups").exists())
        self.assertFalse((self.deploy / "reload-trace").exists())

    # 用途：验证失败恢复使用同一备份且保留尚未恢复成功的门禁。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_reload_failure_rolls_back_and_persistent_failure_keeps_marker(self):
        helper = self.deploy / "bin/provisioner-config-refresh.sh"
        helper.write_text("""refresh_subsystem_control_plane_config() {
  count=0
  [[ ! -f "$deploy_dir/reload-count" ]] || count="$(cat "$deploy_dir/reload-count")"
  count=$((count + 1)); printf '%s' "$count" > "$deploy_dir/reload-count"
  [[ "${FAKE_RELOAD_ALWAYS_FAIL:-false}" != true && "$count" -gt 1 ]]
}
""")
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)
        self.assertEqual((self.deploy / "reload-count").read_text(), "2")
        self.assertFalse((self.deploy / "runtime/.control-plane-reload-required").exists())
        (self.deploy / "reload-count").unlink()
        result = self.invoke(FAKE_RELOAD_ALWAYS_FAIL="true")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)
        self.assertTrue((self.deploy / "runtime/.control-plane-reload-required").exists())
        self.assertIn("恢复重载未完成", result.stderr)

    # 用途：验证未遵守共享锁的外部改写也会被文件CAS挡住。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_stale_current_file_is_not_overwritten(self):
        changed = self.fixture.original + "# externally reviewed update\n"
        docker = self.fixture.tools / "docker"
        docker.write_text("#!/usr/bin/env python3\nfrom pathlib import Path\n" +
                          "Path(" + repr(str(self.deploy / "docker-compose.yml")) + ").write_text(" + repr(changed) + ")\n")
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("并发修改", result.stderr)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), changed)
        self.assertFalse((self.deploy / "reload-trace").exists())

    # 用途：验证可信助手的父目录也不能通过符号链接绕过部署边界。
    # 参数：self为当前临时部署实例。
    # 返回值：无。
    def test_symlinked_bin_directory_is_rejected(self):
        current = self.deploy / "bin"
        saved = self.deploy / "bin.saved"
        current.rename(saved)
        current.symlink_to(saved, target_is_directory=True)
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)

    # 用途：验证候选校验期间来源dist漂移也不能被当成当前批准来源。
    # 参数：self为当前临时部署实例。
    # 返回值：无。
    def test_source_dist_change_during_validation_blocks_commit(self):
        source = self.deploy / "docker-compose.yml.dist"
        changed = source.read_text() + "# external source change\n"
        docker = self.fixture.tools / "docker"
        docker.write_text("#!/usr/bin/env python3\nfrom pathlib import Path\n" +
                          "Path(" + repr(str(source)) + ").write_text(" + repr(changed) + ")\n")
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.fixture.original)
        self.assertFalse((self.deploy / "reload-trace").exists())

    # 用途：验证恢复重载中的外部改图不会使旧SHA检查误清门禁。
    # 参数：self为当前临时部署实例。
    # 返回值：无；恢复后当前文件漂移却被当作完整恢复时失败。
    def test_recovery_time_file_change_keeps_marker(self):
        helper = self.deploy / "bin/provisioner-config-refresh.sh"
        helper.write_text("""refresh_subsystem_control_plane_config() {
  if [[ ! -f "$deploy_dir/recovery-attempted" ]]; then
    touch "$deploy_dir/recovery-attempted"
    return 9
  fi
  printf '# concurrent recovery-time change\\n' >> "$deploy_dir/docker-compose.yml"
  return 0
}
""")
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.deploy / "runtime/.control-plane-reload-required").exists())
        self.assertIn("恢复重载未完成", result.stderr)
        self.assertTrue((self.deploy / "docker-compose.yml").read_text().endswith("# concurrent recovery-time change\n"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
