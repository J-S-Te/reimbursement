#!/usr/bin/env bash
set -Eeuo pipefail

# deploy_reimbursement_remote 将不可变报销镜像交给平台现有受控发布入口。
# 参数：部署根目录及带sha256摘要的阿里云镜像引用。
# 返回值：保留受控发布退出码；参数或前置资产不合法时非零失败。
deploy_reimbursement_remote() {
  [[ $# -eq 2 ]] || {
    echo 'usage: deploy-reimbursement-remote.sh <deploy-dir> <image@sha256:digest>' >&2
    return 2
  }
  local deploy_dir="$1" image_ref="$2" required
  local enterprise='^[a-z0-9.-]+\.cr\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
  local legacy='^registry(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$'
  [[ "$image_ref" =~ $enterprise || "$image_ref" =~ $legacy ]] || {
    echo '拒绝可变或格式错误的报销镜像引用，必须提供阿里云sha256摘要镜像' >&2
    return 2
  }
  [[ -d "$deploy_dir" ]] || {
    echo '报销受控部署目录不存在' >&2
    return 1
  }
  for required in .env .release.env docker-compose.yml bin/deploy-service.sh; do
    [[ -f "$deploy_dir/$required" ]] || {
      echo "缺少报销受控部署资产：$required" >&2
      return 1
    }
  done
  [[ -x "$deploy_dir/bin/deploy-service.sh" ]] || {
    echo '报销受控部署入口不可执行' >&2
    return 1
  }
  cd -- "$deploy_dir"
  # 由平台入口继续执行备份、许可、迁移和回滚，不将首次镜像暂存视为发布完成。
  DEPLOY_FAIL_IF_RUNTIME_NOT_READY=true ./bin/deploy-service.sh reimbursement "$image_ref"
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]:-}" == "$0" ]]; then
  deploy_reimbursement_remote "$@"
fi
