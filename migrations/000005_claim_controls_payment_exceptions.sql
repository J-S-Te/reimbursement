-- 外部付款异常只追加观察事实和独立决定，不能替代正常付款/冲正账本。
CREATE TABLE IF NOT EXISTS exp_payment_exception_fact (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  claim_id VARCHAR(64) NOT NULL,
  recorder VARCHAR(128) NOT NULL,
  method VARCHAR(30) NOT NULL,
  source_account_ref VARCHAR(128) NOT NULL,
  transaction_no VARCHAR(128) NOT NULL,
  content_hash VARCHAR(64) NOT NULL,
  content_json LONGTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uq_exception_transaction (tenant_id, method, source_account_ref, transaction_no),
  KEY idx_exception_claim (claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_payment_exception_cursor (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  claim_id VARCHAR(64) NOT NULL,
  status VARCHAR(30) NOT NULL,
  version BIGINT NOT NULL,
  last_review_id VARCHAR(64) NULL,
  updated_at DATETIME(3) NOT NULL,
  KEY idx_exception_cursor_tenant (tenant_id),
  KEY idx_exception_cursor_claim (claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_payment_exception_review (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  exception_id VARCHAR(64) NOT NULL,
  claim_id VARCHAR(64) NOT NULL,
  actor VARCHAR(128) NOT NULL,
  version BIGINT NOT NULL,
  decision VARCHAR(40) NOT NULL,
  content_hash VARCHAR(64) NOT NULL,
  content_json LONGTEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uq_exception_review_version (tenant_id, exception_id, version),
  KEY idx_exception_review_claim (claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
