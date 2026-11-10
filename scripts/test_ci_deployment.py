#!/usr/bin/env python3
"""使用隔离临时部署目录验证发布边界，不连接 Docker、SSH 或业务库。"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts/deploy-reimbursement-remote.sh"
IMAGE = (
    "test-registry-vpc.cn-hangzhou.cr.aliyuncs.com/"
    "full_lifecycle_management/reimbursement@sha256:" + "a" * 64
)


class RemoteDeploymentTests(unittest.TestCase):
    # 用途：建立仅记录调用参数的隔离部署资产。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reimbursement-ci-")
        self.addCleanup(self.temporary.cleanup)
        self.deploy = Path(self.temporary.name) / "deployment with spaces"
        (self.deploy / "bin").mkdir(parents=True)
        for name in (".env", ".release.env", "docker-compose.yml"):
            (self.deploy / name).touch()
        self.delegate = self.deploy / "bin/deploy-service.sh"
        self.delegate.write_text(
            "#!/usr/bin/env bash\nset -Eeuo pipefail\n"
            "printf '%s\\n' \"$PWD\" \"$DEPLOY_FAIL_IF_RUNTIME_NOT_READY\" "
            "\"$#\" \"$@\" > called.txt\n"
            "exit \"${FAKE_DEPLOY_EXIT:-0}\"\n"
        )
        self.delegate.chmod(0o700)

    # 用途：执行真实远端包装脚本并捕获公开结果。
    # 参数：self为测试实例，args为位置参数，exit_code为模拟受控部署退出码。
    # 返回值：子进程结果，不执行真实部署。
    def invoke(self, *args, exit_code=0):
        environment = dict(os.environ)
        environment["FAKE_DEPLOY_EXIT"] = str(exit_code)
        environment["DEPLOY_FAIL_IF_RUNTIME_NOT_READY"] = "false"
        return subprocess.run(
            ["bash", str(HELPER), *map(str, args)],
            text=True, capture_output=True, env=environment, check=False,
        )

    # 用途：验证仅把不可变摘要交给受控部署并强制启用真实就绪门禁。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_digest_uses_controlled_deploy_with_strict_runtime_flag(self):
        result = self.invoke(self.deploy, IMAGE)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            (self.deploy / "called.txt").read_text().splitlines(),
            [str(self.deploy), "true", "2", "reimbursement", IMAGE],
        )

    # 用途：验证可变tag、错误摘要及非受控registry在调用部署前拒绝。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_invalid_images_never_call_deploy(self):
        for image in (
            IMAGE.split("@")[0] + ":latest", IMAGE[:-1],
            IMAGE.replace("sha256:", "sha512:"),
            "example.com/reimbursement@sha256:" + "a" * 64,
            IMAGE + "\nignored",
        ):
            with self.subTest(image=image):
                result = self.invoke(self.deploy, image)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.deploy / "called.txt").exists())

    # 用途：验证缺少部署目录时显式失败，不创建环境。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_missing_directory_fails(self):
        missing = self.deploy / "missing"
        result = self.invoke(missing, IMAGE)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(missing.exists())

    # 用途：验证平台配置、发布指针、Compose和部署入口缺失均不能发布。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_missing_assets_fail_before_delegate(self):
        for name in (".env", ".release.env", "docker-compose.yml", "bin/deploy-service.sh"):
            with self.subTest(asset=name):
                asset = self.deploy / name
                saved = asset.with_name(asset.name + ".saved")
                asset.rename(saved)
                try:
                    result = self.invoke(self.deploy, IMAGE)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse((self.deploy / "called.txt").exists())
                finally:
                    saved.rename(asset)

    # 用途：验证不可执行部署入口不被包装脚本绕过。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_non_executable_delegate_fails(self):
        self.delegate.chmod(0o600)
        result = self.invoke(self.deploy, IMAGE)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.deploy / "called.txt").exists())

    # 用途：验证备份、许可、迁移或就绪失败不会被包装成成功。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_delegate_failure_is_propagated(self):
        result = self.invoke(self.deploy, IMAGE, exit_code=42)
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertTrue((self.deploy / "called.txt").exists())

    # 用途：验证位置参数数量错误时拒绝执行。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_argument_count_is_enforced(self):
        for args in ((), (self.deploy,), (self.deploy, IMAGE, "extra")):
            with self.subTest(args=args):
                self.assertEqual(self.invoke(*args).returncode, 2)
                self.assertFalse((self.deploy / "called.txt").exists())


class WorkflowBoundaryTests(unittest.TestCase):
    # 用途：验证PR不会发布镜像或部署，发布依赖既有CI和不可变镜像。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_workflow_preserves_release_boundaries(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("workflow_dispatch:", workflow)
        self.assertIn("cancel-in-progress: false", workflow)
        image = workflow.split("\n  image:\n", 1)[-1]
        deploy = workflow.split("\n  deploy:\n", 1)[-1]
        for job in (image, deploy):
            self.assertIn("github.event_name != 'pull_request'", job)
            self.assertIn("github.ref == 'refs/heads/main'", job)
        for required in (
            "needs: backend", "platforms: linux/amd64", "provenance: true",
            "sbom: true", "${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}",
        ):
            self.assertIn(required, image)
        for required in (
            "environment: test", "needs: image", "StrictHostKeyChecking=yes",
            "UserKnownHostsFile=", "chmod 600", "mktemp -d", "trap ",
            "< scripts/deploy-reimbursement-remote.sh",
        ):
            self.assertIn(required, deploy)
        self.assertIn("go-version-file: go.mod", workflow)
        self.assertIn("image: mysql:8.4", workflow)
        self.assertIn("go test -race ./...", workflow)
        self.assertIn("python3 scripts/test_ci_deployment.py", workflow)


if __name__ == "__main__":
    unittest.main(verbosity=2)
