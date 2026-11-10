-- 已发布政策内容保持不可变，退休事实独立追加；发布/替换在租户政策锁下完成。
CREATE TABLE IF NOT EXISTS exp_policy_lifecycle (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  config_id VARCHAR(64) NOT NULL,
  kind VARCHAR(40) NOT NULL,
  operation VARCHAR(20) NOT NULL,
  replacement_id VARCHAR(64) NULL,
  actor VARCHAR(128) NOT NULL,
  reason VARCHAR(2000) NOT NULL,
  previous_version BIGINT NOT NULL,
  current_version BIGINT NOT NULL,
  content_hash VARCHAR(64) NOT NULL,
  effective_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uq_policy_retirement (tenant_id, config_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- 转交仅追加每跳原目标、新目标与目录/本地映射证据，不改写原审批快照。
CREATE TABLE IF NOT EXISTS exp_approval_action_detail (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  action_id VARCHAR(64) NOT NULL,
  task_id VARCHAR(64) NOT NULL,
  claim_id VARCHAR(64) NOT NULL,
  from_subject_id VARCHAR(128) NOT NULL,
  to_subject_id VARCHAR(128) NOT NULL,
  reason VARCHAR(2000) NOT NULL,
  content_json LONGTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uq_transfer_action (action_id),
  KEY idx_transfer_tenant (tenant_id),
  KEY idx_transfer_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
