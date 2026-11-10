#!/usr/bin/env python3
"""仅为受控报销接入追加服务图，不启动业务或修改运行凭据。"""

import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile


MAX_BYTES = 4 * 1024 * 1024
SERVICES = frozenset({"reimbursement-mysql", "reimbursement-migrate",
                      "reimbursement-catalog-sync", "reimbursement-api", "reimbursement-worker"})
VOLUMES = frozenset({"reimbursement-mysql-data", "license-state-reimbursement-api",
                     "license-state-reimbursement-worker"})
PROTECTED_MARKERS = (".assets-install-transaction", ".control-plane-reload-required",
                     ".runtime-approval-publish")
IMAGE = re.compile(r"(?:[a-z0-9.-]+\.cr\.aliyuncs\.com|registry(?:-vpc)?\.[a-z0-9-]+\.aliyuncs\.com)"
                   r"/[a-z0-9._/-]+@sha256:[a-f0-9]{64}\Z")
TOP = re.compile(r"^([A-Za-z][A-Za-z0-9_-]*):\s*(?:#.*)?$")
ENTRY = re.compile(r"^  ([A-Za-z0-9_.-]+):(?:\s.*)?$")


class AdmissionError(Exception):
    pass


def fingerprint(raw):
    """用途：计算不含运行内容的文件摘要。
    参数：raw为原始文件字节。
    返回值：SHA256十六进制摘要。
    """
    return hashlib.sha256(raw).hexdigest()


def read_regular(path):
    """用途：有界读取普通文件并拒绝符号链接。
    参数：path为已限定部署目录内的文件。
    返回值：文件字节及元数据，类型、规模或读取异常时拒绝。
    """
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as source:
            metadata = os.fstat(source.fileno())
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size > MAX_BYTES:
                raise AdmissionError("准入输入不是有界普通文件")
            raw = source.read(MAX_BYTES + 1)
    except OSError as error:
        raise AdmissionError("准入输入缺失、无权读取或不是安全普通文件") from error
    if len(raw) > MAX_BYTES:
        raise AdmissionError("准入输入超过大小上限")
    return raw, metadata


def safe_directory(path, create=False):
    """用途：检查私有事务目录，不跟随目录符号链接。
    参数：path为事务目录，create表示允许首次建立该目录。
    返回值：无，目录不安全或创建失败时拒绝。
    """
    if path.is_symlink() or (path.exists() and not path.is_dir()):
        raise AdmissionError("准入事务目录不安全")
    if create:
        path.mkdir(mode=0o700, parents=False, exist_ok=True)
    if not path.is_dir():
        raise AdmissionError("准入事务目录缺失")


def sections(raw):
    """用途：识别已有块式Compose范围而不重写其余任何字节。
    参数：raw为UTF-8编排字节。
    返回值：原始逐行文本与唯一顶层字段范围，不支持的结构拒绝。
    """
    try:
        lines = raw.decode("utf-8").splitlines(keepends=True)
    except UnicodeError as error:
        raise AdmissionError("Compose不是有效UTF-8文本") from error
    fields = {}
    starts = []
    for index, line in enumerate(lines):
        if "\t" in line:
            raise AdmissionError("Compose含不支持的制表符结构")
        match = TOP.fullmatch(line.rstrip("\r\n"))
        if match:
            key = match.group(1)
            if key in fields:
                raise AdmissionError("Compose存在重复顶层字段")
            fields[key] = None
            starts.append((key, index))
    for position, (key, start) in enumerate(starts):
        end = starts[position + 1][1] if position + 1 < len(starts) else len(lines)
        fields[key] = (start, end)
    if "services" not in fields or "volumes" not in fields:
        raise AdmissionError("Compose缺少受支持的services或volumes块")
    return lines, fields


def entries(lines, bounds):
    """用途：读取唯一服务或卷名称及其原始文本范围。
    参数：lines为原始行集合，bounds为顶层块起止位置。
    返回值：名称及范围映射，重复声明时拒绝。
    """
    result = {}
    start, end = bounds
    previous = None
    for index in range(start + 1, end):
        match = ENTRY.fullmatch(lines[index].rstrip("\r\n"))
        if not match:
            if re.match(r"^  [^ #\r\n]", lines[index]):
                raise AdmissionError("Compose含不支持的直接条目或合并声明")
            continue
        name = match.group(1)
        if name in result:
            raise AdmissionError("Compose存在重复服务或卷声明")
        if previous is not None:
            result[previous][1] = index
        result[name] = [index, end]
        previous = name
    return result


def source_parts(raw):
    """用途：从已受控安装dist精确提取报销五服务与三个独立卷。
    参数：raw为dist原始字节。
    返回值：服务标记块与卷声明；来源结构越界时拒绝。
    """
    lines, fields = sections(raw)
    begin = [i for i, line in enumerate(lines) if re.match(
        r"^  # BEGIN SUBSYSTEM: reimbursement(?:\s|$)", line)]
    end = [i for i, line in enumerate(lines) if re.match(
        r"^  # END SUBSYSTEM: reimbursement(?:\s|$)", line)]
    if len(begin) != 1 or len(end) != 1 or begin[0] >= end[0]:
        raise AdmissionError("受控dist缺少唯一完整报销标记块")
    if not fields["services"][0] < begin[0] < end[0] < fields["services"][1]:
        raise AdmissionError("报销服务块不在services范围内")
    block = lines[begin[0]:end[0] + 1]
    if set(entries(["services:\n"] + block, (0, len(block) + 1))) != SERVICES:
        raise AdmissionError("受控dist的报销服务集合必须恰好为五项")
    available = entries(lines, fields["volumes"])
    if not VOLUMES <= set(available):
        raise AdmissionError("受控dist缺少报销三个独立卷")
    volumes = []
    for name in available:
        if name not in VOLUMES:
            continue
        start, stop = available[name]
        declaration = lines[start:stop]
        if not re.fullmatch(r"  " + re.escape(name) + r":\s*(?:#.*)?", lines[start].rstrip("\r\n")):
            raise AdmissionError("报销独立卷不允许内联配置")
        if any(line.strip() and not line.lstrip().startswith("#") for line in declaration[1:]):
            raise AdmissionError("报销独立卷不允许附带外部存储或别名配置")
        volumes.append(lines[start])
    return "".join(block).encode(), "".join(volumes).encode()


def prepare_candidate(original, source):
    """用途：仅追加固定报销图并保留现有所有文本字节。
    参数：original为现行编排，source为受控dist。
    返回值：候选字节，完整存在返回原字节，部分存在或冲突时拒绝。
    """
    lines, fields = sections(original)
    services = set(entries(lines, fields["services"])) & SERVICES
    volumes = set(entries(lines, fields["volumes"])) & VOLUMES
    if services == SERVICES and volumes == VOLUMES:
        return original
    if services or volumes or re.search(rb"SUBSYSTEM:\s*reimbursement(?:\s|$)", original):
        raise AdmissionError("报销服务或卷部分存在，拒绝覆盖既有配置")
    if not original.endswith(b"\n"):
        raise AdmissionError("现有Compose缺少终止换行，拒绝隐式改写")
    block, declarations = source_parts(source)
    inserts = [(fields["services"][1], block.decode()),
               (fields["volumes"][1], declarations.decode())]
    for index, content in sorted(inserts, reverse=True):
        lines[index:index] = [content]
    return "".join(lines).encode()


def run_private(command, environment, timeout, stdin=None):
    """用途：运行有界外部验证并禁止配置或错误中的敏感值输出。
    参数：command为固定命令，environment为继承配置，timeout为秒，stdin为受控脚本。
    返回值：命令成功与否；超时终止整个CLI进程组后返回失败。
    """
    process = subprocess.Popen(command, env=environment, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    try:
        process.communicate(stdin, timeout=timeout)
        return process.returncode == 0
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.communicate()
        return False


def validate_compose(root, candidate, environment):
    """用途：用真实Compose仅解析候选配置，不创建网络或启动服务。
    参数：root为部署根，candidate为候选文件，environment绑定本次不可变报销镜像。
    返回值：无，解析失败仅返回脱敏错误。
    """
    command = ["docker", "compose", "--project-directory", str(root), "--file", str(candidate),
               "--env-file", str(root / ".env"), "--env-file", str(root / ".release.env"),
               "config", "--quiet"]
    if not run_private(command, environment, 30):
        raise AdmissionError("报销Compose候选验证失败，现行配置未被替换")


def reload_control_plane(root, environment):
    """用途：在已持有共享锁时复用受控安装的成对重载与摘要健康门禁。
    参数：root为部署根，environment为本次镜像环境；不调用任何报销业务启动命令。
    返回值：既有Agent/API双SHA、健康、socket及profile验证是否全部通过。
    """
    script = '''set -Eeuo pipefail
deploy_dir="$1"
runtime_file="$deploy_dir/.env"
release_file="$deploy_dir/.release.env"
compose_file="$deploy_dir/docker-compose.yml"
profiles_dir="$deploy_dir/subsystems.d"
COMPOSE_PROJECT_NAME="$(awk -F= '$1=="COMPOSE_PROJECT_NAME" {n++; v=substr($0,index($0,"=")+1)} END {if(n>1) exit 1; print v}' "$runtime_file")"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME#\\\"}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME%\\\"}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME#\\'}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME%\\'}"
# compose 只调用现有服务图，避免候选预检意外创建网络。
# 参数：现有受控重载函数传入的Compose命令参数。
# 返回值：Docker Compose原始退出码。
compose() {
  docker compose --project-directory "$deploy_dir" --file "$compose_file" \\
    --env-file "$runtime_file" --env-file "$release_file" "$@"
}
source "$deploy_dir/bin/provisioner-config-refresh.sh"
refresh_subsystem_control_plane_config
'''.encode("utf-8")
    return run_private(["bash", "-s", "--", str(root)], environment, 300, script)


def verify_ready(root, image):
    """用途：发布后只读核验API和Worker容器内真实HTTP就绪状态。
    参数：root为部署目录，image为本轮不可变镜像环境，不读取或输出运行凭据。
    返回值：两组件HTTP与JSON均ready才成功，失败明确可恢复且不伪称数据库回滚。
    """
    environment = dict(os.environ, REIMBURSEMENT_IMAGE=image)
    if not IMAGE.fullmatch(image) or root.is_symlink() or not root.is_dir():
        raise AdmissionError("就绪核验参数不安全")
    for service, port in (("reimbursement-api", 8087), ("reimbursement-worker", 9097)):
        command = ["docker", "compose", "--project-directory", str(root),
                   "--file", str(root / "docker-compose.yml"),
                   "--env-file", str(root / ".env"), "--env-file", str(root / ".release.env"),
                   "exec", "-T", service, "wget", "-q", "-T", "10", "-O", "-",
                   f"http://127.0.0.1:{port}/readyz"]
        try:
            result = subprocess.run(command, env=environment, capture_output=True, timeout=15)
            value = json.loads(result.stdout) if result.returncode == 0 and len(result.stdout) <= 32768 else None
        except (subprocess.TimeoutExpired, ValueError, OSError):
            value = None
        if not isinstance(value, dict) or value.get("status") != "ready":
            raise AdmissionError(f"上游发布已执行，但{service}未就绪；保留现场供受控恢复，未执行数据库回滚")
    return {"status": "READY", "components": ["reimbursement-api", "reimbursement-worker"]}


def atomic_file(path, raw, metadata=None, exclusive=False):
    """用途：同目录持久化后原子发布文件，不写穿符号链接。
    参数：path为目标，raw为字节，metadata为原属主权限，exclusive禁止替换已有标记。
    返回值：无，写入或目录同步失败时保留外层恢复门禁。
    """
    if path.is_symlink():
        raise AdmissionError("原子写入目标不能是符号链接")
    fd, name = tempfile.mkstemp(prefix=".reimbursement-compose-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as destination:
            destination.write(raw)
            destination.flush()
            if metadata is not None:
                current = os.fstat(destination.fileno())
                if (current.st_uid, current.st_gid) != (metadata.st_uid, metadata.st_gid):
                    os.fchown(destination.fileno(), metadata.st_uid, metadata.st_gid)
            os.fchmod(destination.fileno(), stat.S_IMODE(metadata.st_mode) if metadata else 0o600)
            os.fsync(destination.fileno())
        if exclusive:
            os.link(temporary, path)
        else:
            os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


def create_backup(root, original):
    """用途：在改图前创建0600原文件备份及绑定SHA并实际复核。
    参数：root为部署根，original为锁内原文件字节。
    返回值：仅含本次备份的私有目录，备份失败禁止继续。
    """
    safe_directory(root / "backups", create=True)
    directory = Path(tempfile.mkdtemp(prefix="reimbursement-compose-", dir=root / "backups"))
    directory.chmod(0o700)
    atomic_file(directory / "docker-compose.yml", original, exclusive=True)
    digest = fingerprint(original)
    atomic_file(directory / "docker-compose.yml.sha256",
                (digest + "  docker-compose.yml\n").encode(), exclusive=True)
    saved, _ = read_regular(directory / "docker-compose.yml")
    sidecar, _ = read_regular(directory / "docker-compose.yml.sha256")
    if saved != original or sidecar != (digest + "  docker-compose.yml\n").encode():
        raise AdmissionError("报销Compose备份摘要复核失败")
    return directory


def commit_candidate(root, original, candidate, metadata, environment, source):
    """用途：原子准入、重载及失败恢复，全部受同一共享锁保护。
    参数：root为部署根，original/candidate为文件，metadata为原属性，environment为环境，source为来源。
    返回值：公开摘要回执；失败恢复仍不成功时保留marker和备份并拒绝发布。
    """
    current = root / "docker-compose.yml"
    if read_regular(root / "docker-compose.yml.dist")[0] != source:
        raise AdmissionError("准入期间dist来源已改变，拒绝使用过期来源")
    backup = create_backup(root, original)
    marker = root / "runtime/.control-plane-reload-required"
    before, after = fingerprint(original), fingerprint(candidate)
    if read_regular(current)[0] != original:
        raise AdmissionError("准入期间Compose已被并发修改，拒绝覆盖")
    content = ("FORMAT=1\nREASON=reimbursement_compose_admission\n"
               f"INSTALLED_AT_UTC={datetime.datetime.now(datetime.timezone.utc).isoformat()}\n"
               f"COMPOSE_BEFORE_SHA256={before}\nCOMPOSE_AFTER_SHA256={after}\n"
               f"COMPOSE_SOURCE_SHA256={fingerprint(source)}\n"
               f"COMPOSE_BACKUP={backup.relative_to(root)}\n").encode()
    atomic_file(marker, content, exclusive=True)
    try:
        atomic_file(current, candidate, metadata)
        if not reload_control_plane(root, environment):
            raise AdmissionError("报销Compose准入后的控制面重载失败")
        if read_regular(current)[0] != candidate or read_regular(marker)[0] != content:
            raise AdmissionError("准入重载后文件或marker发生漂移")
        marker.unlink()
    except (Exception, KeyboardInterrupt) as error:
        if read_regular(current)[0] not in (candidate, original):
            raise AdmissionError("恢复被并发改动阻断，已保留marker与备份") from error
        atomic_file(current, original, metadata)
        restored = read_regular(current)[0] == original and reload_control_plane(root, environment)
        restored = (restored and read_regular(current)[0] == original and
                    read_regular(marker)[0] == content)
        if restored:
            marker.unlink()
        raise AdmissionError("准入失败，已恢复原Compose" if restored else
                             "准入失败且恢复重载未完成，已保留marker与备份") from error
    return {"status": "ADMITTED", "before_sha256": before, "after_sha256": after,
            "source_sha256": fingerprint(source), "backup": str(backup.relative_to(root))}


def admit(root, image):
    """用途：在共享锁中为唯一报销系统准备完整服务图，不修改接入或许可事实。
    参数：root为现有部署目录，image为本轮不可变阿里云镜像引用。
    返回值：幂等或准入回执，任何不安全输入及依赖失败都拒绝。
    """
    if not IMAGE.fullmatch(image) or root.is_symlink() or not root.is_dir() or root == Path("/"):
        raise AdmissionError("报销准入参数不安全")
    root = root.resolve()
    safe_directory(root / "bin")
    for name in (".env", ".release.env", "bin/provisioner-config-refresh.sh"):
        read_regular(root / name)
    safe_directory(root / "runtime", create=True)
    lock = os.open(root / "runtime/.deploy.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        if not stat.S_ISREG(os.fstat(lock).st_mode):
            raise AdmissionError("发布锁不是普通文件")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise AdmissionError("已有发布或平台接入正在运行，拒绝并发准入") from error
        for name in PROTECTED_MARKERS:
            marker = root / "runtime" / name
            if marker.exists() or marker.is_symlink():
                raise AdmissionError("存在未完成的受控事务，须先按原流程恢复")
        original, metadata = read_regular(root / "docker-compose.yml")
        lines, fields = sections(original)
        complete = (SERVICES <= set(entries(lines, fields["services"])) and
                    VOLUMES <= set(entries(lines, fields["volumes"])))
        source = b"" if complete else read_regular(root / "docker-compose.yml.dist")[0]
        candidate = prepare_candidate(original, source)
        environment = dict(os.environ, REIMBURSEMENT_IMAGE=image)
        fd, name = tempfile.mkstemp(prefix=".reimbursement-compose-check-", dir=root)
        os.close(fd)
        try:
            Path(name).write_bytes(candidate)
            validate_compose(root, Path(name), environment)
        finally:
            Path(name).unlink(missing_ok=True)
        if candidate == original:
            return {"status": "UNCHANGED", "sha256": fingerprint(original)}
        return commit_candidate(root, original, candidate, metadata, environment, source)
    finally:
        os.close(lock)


def main():
    """用途：提供仅接受部署目录和不可变镜像的窄准入命令。
    参数：无；从命令行接收两个固定位置参数。
    返回值：成功输出非敏感摘要并返回0，失败输出脱敏原因并返回1。
    """
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("deploy_dir", type=Path)
    parser.add_argument("image")
    parser.add_argument("--verify-ready", action="store_true")
    args = parser.parse_args()
    try:
        result = verify_ready(args.deploy_dir, args.image) if args.verify_ready else admit(args.deploy_dir, args.image)
        print(json.dumps(result, ensure_ascii=False))
        return 0
    except (AdmissionError, OSError, ValueError) as error:
        message = str(error) if isinstance(error, AdmissionError) else "准入文件或系统操作失败"
        print(message, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
