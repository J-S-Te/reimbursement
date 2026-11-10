-- 更正与风险保留独立案件，旧审批快照与付款账本不回写。
CREATE TABLE IF NOT EXISTS exp_invoice_correction (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 invoice_id VARCHAR(64) NOT NULL,
 requester VARCHAR(128) NOT NULL,
 approver VARCHAR(128) NOT NULL DEFAULT '',
 status VARCHAR(30) NOT NULL,
 version BIGINT NOT NULL,
 invoice_version BIGINT NOT NULL,
 content_json LONGTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 KEY idx_invoice_correction_tenant (tenant_id),
 KEY idx_invoice_correction_invoice (invoice_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_invoice_control_impact (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 invoice_id VARCHAR(64) NOT NULL,
 control_id VARCHAR(64) NOT NULL,
 claim_id VARCHAR(64) NOT NULL,
 kind VARCHAR(30) NOT NULL,
 status VARCHAR(40) NOT NULL,
 version BIGINT NOT NULL,
 original_round INT NOT NULL,
 invoice_version BIGINT NOT NULL,
 confirmed_by VARCHAR(128) NOT NULL DEFAULT '',
 content_json LONGTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_invoice_control_claim (control_id, claim_id),
 KEY idx_invoice_impact_tenant (tenant_id),
 KEY idx_invoice_impact_invoice (invoice_id),
 KEY idx_invoice_impact_claim (claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_invoice_risk (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 invoice_id VARCHAR(64) NOT NULL,
 original_invoice_id VARCHAR(64) NOT NULL,
 recorder VARCHAR(128) NOT NULL,
 reviewer VARCHAR(128) NOT NULL DEFAULT '',
 kind VARCHAR(30) NOT NULL,
 status VARCHAR(30) NOT NULL,
 resolution VARCHAR(40) NOT NULL DEFAULT '',
 version BIGINT NOT NULL,
 content_json LONGTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 KEY idx_invoice_risk_tenant (tenant_id),
 KEY idx_invoice_risk_invoice (invoice_id),
 KEY idx_invoice_risk_original (original_invoice_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
