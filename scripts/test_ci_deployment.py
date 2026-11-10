#!/usr/bin/env python3
"""使用隔离临时部署目录验证发布边界，不连接 Docker、SSH 或业务库。"""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
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
        self.original = (
            "services:\n  platform-api:\n    image: platform-original\n"
            "  subsystem-provisioner:\n    image: agent-original\n"
            "networks:\n  application:\nvolumes:\n  existing-volume:\n"
        )
        (self.deploy / "docker-compose.yml").write_text(self.original)
        names = ("reimbursement-mysql", "reimbursement-migrate", "reimbursement-catalog-sync",
                 "reimbursement-api", "reimbursement-worker")
        block = "  # BEGIN SUBSYSTEM: reimbursement — 开始\n"
        block += "".join(f"  {name}:\n    image: ${{REIMBURSEMENT_IMAGE:-pending}}\n" for name in names)
        block += "  # END SUBSYSTEM: reimbursement — 结束\n"
        self.block = block
        self.volumes = ("  reimbursement-mysql-data:\n"
                        "  license-state-reimbursement-api:\n"
                        "  license-state-reimbursement-worker:\n")
        (self.deploy / "docker-compose.yml.dist").write_text(
            "services:\n" + block + "networks:\n  application:\nvolumes:\n" + self.volumes
        )
        (self.deploy / "bin/provisioner-config-refresh.sh").write_text(
            "refresh_subsystem_control_plane_config() {\n"
            "  printf 'controlled-reload\\n' >> \"$deploy_dir/reload-trace\"\n"
            "  return 0\n}\n"
        )
        self.tools = Path(self.temporary.name) / "tools"
        self.tools.mkdir()
        docker = self.tools / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\nimport json,os,sys\nfrom pathlib import Path\n"
            "with open(os.environ['FAKE_DOCKER_TRACE'],'a') as out: out.write(json.dumps(sys.argv[1:])+'\\n')\n"
            "if sys.argv[-2:]==['config','--quiet']: sys.exit(0)\n"
            "assert 'exec' in sys.argv, 'unexpected Docker mutation'\n"
            "mode=os.environ.get('FAKE_READY_MODE','ready')\n"
            "if mode=='api-503' and 'reimbursement-api' in sys.argv: sys.exit(8)\n"
            "if mode=='worker-503' and 'reimbursement-worker' in sys.argv: sys.exit(8)\n"
            "if mode=='invalid-json': print('<html>SPA</html>')\n"
            "elif mode=='not-ready': print('{\"status\":\"not_ready\"}')\n"
            "else: print('{\"status\":\"ready\"}')\n"
        )
        docker.chmod(0o700)
        self.delegate = self.deploy / "bin/deploy-service.sh"
        self.delegate.write_text(
            "#!/usr/bin/env bash\nset -Eeuo pipefail\n"
            "if [[ \"${1:-}\" == --capabilities ]]; then\n"
            "  echo REIMBURSEMENT_PREPARE_ONLY_V1; exit 0\nfi\n"
            "printf '%s\\n' \"${DEPLOY_PREPARE_ONLY-unset}\" > prepare-flag.txt\n"
            "printf '%s\\n' \"$PWD\" \"$DEPLOY_FAIL_IF_RUNTIME_NOT_READY\" "
            "\"$#\" \"$@\" > called.txt\n"
            "exit \"${FAKE_DEPLOY_EXIT:-0}\"\n"
        )
        self.delegate.chmod(0o700)

    # 用途：执行真实远端包装脚本并捕获公开结果。
    # 参数：self为测试实例，args为位置参数，exit_code为委托退出码，ready_mode为真实HTTP边界替身。
    # 返回值：子进程结果，不执行真实部署。
    def invoke(self, *args, exit_code=0, ready_mode="ready"):
        environment = dict(os.environ)
        environment["FAKE_DEPLOY_EXIT"] = str(exit_code)
        environment["DEPLOY_FAIL_IF_RUNTIME_NOT_READY"] = "false"
        environment["PATH"] = str(self.tools) + os.pathsep + environment["PATH"]
        environment["FAKE_DOCKER_TRACE"] = str(self.deploy / "docker-trace")
        environment["FAKE_READY_MODE"] = ready_mode
        return subprocess.run(
            ["bash", str(HELPER), *map(str, args)],
            text=True, capture_output=True, env=environment, check=False,
        )

    # 用途：验证首次发布在委托前只追加报销图，保留全部现有配置字节。
    # 参数：self为当前临时部署测试实例。
    # 返回值：无；缺少准入、覆盖既有配置或提前启动业务时失败。
    def test_missing_graph_is_admitted_before_strict_deploy(self):
        before_env = (self.deploy / ".env").read_bytes()
        result = self.invoke(self.deploy, IMAGE)
        self.assertEqual(result.returncode, 0, result.stderr)
        current = (self.deploy / "docker-compose.yml").read_text()
        self.assertIn("  reimbursement-api:\n", current)
        self.assertEqual(current.replace(self.block, "").replace(self.volumes, ""), self.original)
        self.assertEqual((self.deploy / ".env").read_bytes(), before_env)
        self.assertTrue((self.deploy / "called.txt").exists())
        self.assertTrue((self.deploy / "reload-trace").exists())

    # 用途：验证上游发布返回成功也必须拒绝两组件未就绪、HTTP失败或SPA响应。
    # 参数：self为当前临时部署实例。
    # 返回值：无；把未就绪当成功或宣称数据库自动回滚时失败。
    def test_readiness_failure_after_delegate_is_not_success(self):
        for mode in ("api-503", "worker-503", "invalid-json", "not-ready"):
            with self.subTest(mode=mode):
                result = self.invoke(self.deploy, IMAGE, ready_mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue((self.deploy / "called.txt").exists())
                self.assertIn("未就绪", result.stderr)
                self.assertNotIn("数据库已回滚", result.stdout + result.stderr)

    # 用途：验证仅把不可变摘要交给受控部署并强制启用真实就绪门禁。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_digest_uses_controlled_deploy_with_strict_runtime_flag(self):
        result = self.invoke(self.deploy, IMAGE)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.deploy / "prepare-flag.txt").read_text(), "false\n")
        self.assertEqual(
            (self.deploy / "called.txt").read_text().splitlines(),
            [str(self.deploy), "true", "2", "reimbursement", IMAGE],
        )
        calls = [json.loads(line) for line in (self.deploy / "docker-trace").read_text().splitlines()]
        readiness = [call for call in calls if "exec" in call]
        self.assertEqual([call[call.index("exec") + 2] for call in readiness],
                         ["reimbursement-api", "reimbursement-worker"])
        self.assertEqual([call[-1] for call in readiness],
                         ["http://127.0.0.1:8087/readyz", "http://127.0.0.1:9097/readyz"])

    # 用途：验证显式准备只暂存候选，保留严格参数且不核验业务就绪。
    # 参数：self为当前隔离测试实例。
    # 返回值：无；未传平台暂存标志或把准备当部署时失败。
    def test_prepare_stages_only_with_explicit_capability(self):
        result = self.invoke(self.deploy, IMAGE, "prepare", ready_mode="api-503")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PREPARED", result.stdout)
        self.assertEqual((self.deploy / "prepare-flag.txt").read_text(), "true\n")
        self.assertEqual((self.deploy / "called.txt").read_text().splitlines()[1], "true")
        calls = (self.deploy / "docker-trace").read_text()
        self.assertNotIn('"exec"', calls)

    # 用途：验证旧平台不支持能力时准备请求在图准入之前失败。
    # 参数：self为当前隔离测试实例。
    # 返回值：无；旧脚本被当作支持暂存或改写Compose时失败。
    def test_prepare_rejects_old_platform_before_any_mutation(self):
        self.delegate.write_text("#!/usr/bin/env bash\nexit 0\n")
        result = self.invoke(self.deploy, IMAGE, "prepare")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("暂存能力", result.stderr)
        self.assertEqual((self.deploy / "docker-compose.yml").read_text(), self.original)
        self.assertFalse((self.deploy / "called.txt").exists())

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
    # 用途：执行真实workflow传输段，验证固定双文件和两种模式均正确到达远端。
    # 参数：self为当前测试实例，SSH与Docker均为隔离边界替身。
    # 返回值：无；遗漏Python、路径转义失败或严格发布未到达时失败。
    def test_workflow_transfers_both_helpers_before_deploy(self):
        fixture = RemoteDeploymentTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        ssh = fixture.tools / "ssh"
        ssh.write_text("#!/usr/bin/env python3\nimport os,subprocess,sys\n"
                       "sys.exit(subprocess.run(['bash','-c',sys.argv[-1]],stdin=sys.stdin,env=os.environ).returncode)\n")
        ssh.chmod(0o700)
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        script = textwrap.dedent(workflow.split("name: Deploy immutable reimbursement image", 1)[1].split("run: |", 1)[1])
        environment = dict(os.environ, DEPLOY_HOST="fixture.invalid", DEPLOY_USER="fixture",
                           DEPLOY_PORT="22", DEPLOY_SSH_KEY="synthetic-key",
                           DEPLOY_KNOWN_HOSTS="synthetic-host", DEPLOY_PATH=str(fixture.deploy),
                           IMAGE_REF=IMAGE, FAKE_DOCKER_TRACE=str(fixture.deploy / "docker-trace"))
        environment["PATH"] = str(fixture.tools) + os.pathsep + environment["PATH"]
        for phase in ("deploy", "prepare"):
            with self.subTest(phase=phase):
                environment["DEPLOY_PHASE"] = phase
                result = subprocess.run(["bash", "-c", script], cwd=ROOT, env=environment,
                                        capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue((fixture.deploy / "called.txt").exists(), result.stdout + result.stderr)
                self.assertEqual((fixture.deploy / "prepare-flag.txt").read_text(),
                                 "true\n" if phase == "prepare" else "false\n")
                self.assertIn("reimbursement-api:", (fixture.deploy / "docker-compose.yml").read_text())
        (fixture.deploy / "called.txt").unlink()
        environment["DEPLOY_PHASE"] = "prepare; touch injected"
        result = subprocess.run(["bash", "-c", script], cwd=ROOT, env=environment,
                                capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 2)
        self.assertFalse((fixture.deploy / "called.txt").exists())

    # 用途：验证PR不会发布镜像或部署，发布依赖既有CI和不可变镜像。
    # 参数：self为当前测试实例。
    # 返回值：无。
    def test_workflow_preserves_release_boundaries(self):
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("workflow_dispatch:", workflow)
        self.assertIn("default: deploy", workflow)
        self.assertIn("github.event_name == 'workflow_dispatch' && inputs.phase || 'deploy'", workflow)
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
            "reimbursement-compose-admission.py", "tar -czf -",
        ):
            self.assertIn(required, deploy)
        self.assertIn("go-version-file: go.mod", workflow)
        self.assertIn("image: mysql:8.4", workflow)
        self.assertIn("go test -race ./...", workflow)
        self.assertIn("python3 scripts/test_ci_deployment.py", workflow)


if __name__ == "__main__":
    unittest.main(verbosity=2)
