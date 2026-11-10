CREATE TABLE IF NOT EXISTS exp_oidc_login_transaction (
  state_hash BINARY(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  nonce_ciphertext VARBINARY(512) NOT NULL,
  code_verifier_ciphertext VARBINARY(1024) NOT NULL,
  return_path VARCHAR(512) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  consumed_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (state_hash),
  KEY idx_exp_oidc_login_tenant_expiry (tenant_id, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS exp_oidc_session (
  session_id_hash BINARY(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  identity_id VARCHAR(128) NOT NULL,
  person_id VARCHAR(64) NOT NULL DEFAULT '',
  principal_json JSON NOT NULL,
  id_token_ciphertext MEDIUMBLOB NOT NULL,
  oauth_token_ciphertext MEDIUMBLOB NOT NULL,
  authorization_revision BIGINT UNSIGNED NOT NULL,
  authorization_checked_at DATETIME(3) NOT NULL,
  refresh_retry_at DATETIME(3) NULL,
  token_expires_at DATETIME(3) NOT NULL,
  session_expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  last_seen_at DATETIME(3) NOT NULL,
  revoked_at DATETIME(3) NULL,
  PRIMARY KEY (session_id_hash),
  KEY idx_exp_oidc_session_subject (tenant_id, identity_id, revoked_at),
  KEY idx_exp_oidc_session_expiry (session_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE exp_oidc_session
  ADD COLUMN oidc_subject VARCHAR(128) NULL AFTER identity_id,
  ADD COLUMN oidc_session_id VARCHAR(128) NULL AFTER oidc_subject,
  ADD KEY idx_exp_oidc_session_sid (tenant_id, oidc_session_id, revoked_at),
  ADD KEY idx_exp_oidc_session_oidc_subject (tenant_id, oidc_subject, revoked_at);

CREATE TABLE IF NOT EXISTS exp_oidc_backchannel_logout_replay (
  jti VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  issuer VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  audience VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  consumed_at DATETIME(3) NOT NULL,
  PRIMARY KEY (jti),
  KEY idx_exp_oidc_backchannel_replay_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE exp_oidc_login_transaction
    ADD COLUMN central_ticket_cipher VARBINARY(1024) NULL,
    ADD COLUMN force_login BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE exp_oidc_session
    ADD COLUMN central_binding_cipher VARBINARY(1024) NULL,
    ADD COLUMN central_session_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL;

-- 统一业务子系统会话（2026-10-30）：空闲 30 分钟 + 绝对 8 小时。
-- 只新增显式交互时间列：last_seen_at 仍表示最近一次业务请求，last_interactive_at 只由
-- POST /api/v1/auth/activity 推进，用于计算空闲期限。
-- 存量行保持 NULL，由代码按 created_at 解释（升级不锁死在线会话）；不修改其它列，
-- 也不回填或改写既有会话的绝对到期时间。
ALTER TABLE exp_oidc_session
  ADD COLUMN last_interactive_at DATETIME(3) NULL AFTER last_seen_at;
