-- 集成恢复只追加命令与回执，不重写付款效果和财务版本。
CREATE TABLE IF NOT EXISTS exp_recovery_command (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 actor VARCHAR(128) NOT NULL,
 command_id VARCHAR(128) NOT NULL,
 resource_id VARCHAR(64) NOT NULL,
 kind VARCHAR(30) NOT NULL,
 reason VARCHAR(1000) NOT NULL,
 request_hash VARCHAR(64) NOT NULL,
 result_json LONGTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_recovery_command (tenant_id, actor, command_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_delivery_receipt (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 outbox_id VARCHAR(64) NOT NULL,
 topic VARCHAR(80) NOT NULL,
 status VARCHAR(30) NOT NULL,
 receipt_json LONGTEXT NOT NULL,
 accepted_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_delivery_outbox (outbox_id),
 KEY idx_delivery_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_reconciliation_cursor (
 tenant_id VARCHAR(64) NOT NULL PRIMARY KEY,
 run_id VARCHAR(64) NOT NULL DEFAULT '',
 mode VARCHAR(20) NOT NULL DEFAULT '',
 phase VARCHAR(20) NOT NULL,
 remote_cursor VARCHAR(512) NOT NULL DEFAULT '',
 source_updated_at DATETIME(3) NOT NULL,
 source_claim_id VARCHAR(64) NOT NULL DEFAULT '',
 watermark DATETIME(3) NOT NULL,
 run_started_at DATETIME(3) NOT NULL,
 last_completed_at DATETIME(3) NULL,
 last_full_at DATETIME(3) NULL,
 last_attempt_at DATETIME(3) NULL,
 last_error VARCHAR(100) NOT NULL DEFAULT '',
 version BIGINT NOT NULL,
 updated_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_reconciliation_finding (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 claim_id VARCHAR(64) NOT NULL,
 run_id VARCHAR(64) NOT NULL,
 source_revision BIGINT UNSIGNED NOT NULL,
 source_hash VARCHAR(64) NOT NULL DEFAULT '',
 remote_revision BIGINT UNSIGNED NOT NULL,
 remote_hash VARCHAR(64) NOT NULL DEFAULT '',
 remote_consistent BOOLEAN NOT NULL DEFAULT FALSE,
 status VARCHAR(40) NOT NULL,
 version BIGINT NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_reconciliation_claim (tenant_id, claim_id),
 KEY idx_reconciliation_status (tenant_id, status, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE INDEX idx_claim_reconciliation_cursor ON exp_claim (tenant_id, updated_at, id);
