#!/usr/bin/env bash
set -Eeuo pipefail

# deploy_reimbursement_remote 先安全准入服务图，再严格发布或受控暂存镜像。
# 参数：部署根目录、不可变阿里云镜像及可选deploy/prepare模式。
# 返回值：保留受控发布退出码；参数或前置资产不合法时非零失败。
deploy_reimbursement_remote() {
  [[ $# -eq 2 || $# -eq 3 ]] || {
    echo 'usage: deploy-reimbursement-remote.sh <deploy-dir> <image@sha256:digest> [deploy|prepare]' >&2
    return 2
  }
  local deploy_dir="$1" image_ref="$2" mode="${3-deploy}" required admission capability
  case "$mode" in deploy|prepare) ;; *) echo '无效报销发布模式' >&2; return 2 ;; esac
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
    [[ -f "$deploy_dir/$required" && ! -L "$deploy_dir/$required" ]] || {
      echo "缺少报销受控部署资产：$required" >&2
      return 1
    }
  done
  [[ -x "$deploy_dir/bin/deploy-service.sh" ]] || {
    echo '报销受控部署入口不可执行' >&2
    return 1
  }
  if [[ "$mode" == prepare ]]; then
    capability="$("$deploy_dir/bin/deploy-service.sh" --capabilities 2>/dev/null)" || {
      echo '平台缺少报销暂存能力，必须先升级受控发布资产' >&2
      return 1
    }
    [[ "$capability" == REIMBURSEMENT_PREPARE_ONLY_V1 ]] || {
      echo '平台暂存能力版本不匹配，拒绝执行旧发布流程' >&2
      return 1
    }
  fi
  admission="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/reimbursement-compose-admission.py"
  [[ -f "$admission" && ! -L "$admission" ]] || {
    echo '缺少可信报销Compose准入助手' >&2
    return 1
  }
  python3 -B "$admission" "$deploy_dir" "$image_ref" || return
  cd -- "$deploy_dir"
  # 由平台入口继续执行备份、许可、迁移和回滚，不将首次镜像暂存视为发布完成。
  if [[ "$mode" == prepare ]]; then
    DEPLOY_PREPARE_ONLY=true DEPLOY_FAIL_IF_RUNTIME_NOT_READY=true \
      ./bin/deploy-service.sh reimbursement "$image_ref" || return
    echo 'PREPARED: 报销镜像候选已受控暂存；未部署业务，仍需完成应用接入与严格发布'
    return 0
  fi
  DEPLOY_PREPARE_ONLY=false DEPLOY_FAIL_IF_RUNTIME_NOT_READY=true \
    ./bin/deploy-service.sh reimbursement "$image_ref" || return
  python3 -B "$admission" "$deploy_dir" "$image_ref" --verify-ready
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]:-}" == "$0" ]]; then
  deploy_reimbursement_remote "$@"
fi
