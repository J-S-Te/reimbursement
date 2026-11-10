-- 报销 V1 本地事务事实与不可变账本。
-- 可变聚合内容保存为受版本保护 JSON；审批、效果、付款、命令、任务独立关系表。
-- exp_claim_reference 仅为项目检索索引，内容权威仍为聚合快照。
-- 不包含身份会话表；禁止生产启动时 AutoMigrate。

CREATE TABLE IF NOT EXISTS `exp_claim` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `applicant` VARCHAR(128) NOT NULL,
  `org_id` VARCHAR(64) NOT NULL,
  `status` VARCHAR(40) NOT NULL,
  `version` BIGINT NOT NULL,
  `active_payment_id` VARCHAR(64) NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  KEY `idx_claim_owner` (`tenant_id`, `applicant`, `status`),
  UNIQUE KEY `idx_exp_claim_active_payment_id` (`active_payment_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_invoice` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `owner` VARCHAR(128) NOT NULL,
  `identity_key` VARCHAR(191) NULL,
  `version` BIGINT NULL,
  `reservation_claim_id` VARCHAR(64) NULL,
  `reservation_state` VARCHAR(20) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_invoice_identity` (`tenant_id`, `identity_key`),
  KEY `idx_exp_invoice_owner` (`owner`),
  KEY `idx_exp_invoice_reservation_claim_id` (`reservation_claim_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_claim_snapshot` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `round` BIGINT NOT NULL,
  `content_hash` VARCHAR(64) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_claim_round` (`tenant_id`, `claim_id`, `round`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_approval_task` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `round` BIGINT NULL,
  `node` BIGINT NULL,
  `node_code` VARCHAR(40) NULL,
  `assignee` VARCHAR(128) NULL,
  `status` VARCHAR(20) NULL,
  `version` BIGINT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  KEY `idx_task_inbox` (`tenant_id`, `assignee`, `status`),
  KEY `idx_exp_approval_task_claim_id` (`claim_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_approval_action` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NULL,
  `task_id` VARCHAR(64) NULL,
  `actor` VARCHAR(128) NULL,
  `action` VARCHAR(40) NULL,
  `reason` VARCHAR(2000) NULL,
  `round` BIGINT NULL,
  `created_at` DATETIME(3) NULL,
  KEY `idx_exp_approval_action_claim_id` (`claim_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_command_dedup` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `actor` VARCHAR(128) NOT NULL,
  `operation` VARCHAR(60) NOT NULL,
  `command_id` VARCHAR(128) NOT NULL,
  `request_hash` VARCHAR(64) NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `result_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_command` (`tenant_id`, `actor`, `operation`, `command_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_payment` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `actor` VARCHAR(128) NOT NULL,
  `method` VARCHAR(30) NOT NULL,
  `source_account_ref` VARCHAR(128) NOT NULL,
  `transaction_no` VARCHAR(128) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_payment_transaction` (`tenant_id`, `method`, `source_account_ref`, `transaction_no`),
  KEY `idx_exp_payment_claim_id` (`claim_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_payment_adjustment_request` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `payment_id` VARCHAR(64) NOT NULL,
  `requester` VARCHAR(128) NOT NULL,
  `status` VARCHAR(20) NOT NULL,
  `version` BIGINT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  KEY `idx_exp_payment_adjustment_request_claim_id` (`claim_id`),
  KEY `idx_exp_payment_adjustment_request_payment_id` (`payment_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_payment_reversal` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `payment_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_reversal_payment` (`tenant_id`, `payment_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_payment_effect` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `payment_id` VARCHAR(64) NOT NULL,
  `kind` VARCHAR(40) NOT NULL,
  `content_hash` VARCHAR(64) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  KEY `idx_exp_payment_effect_claim_id` (`claim_id`),
  UNIQUE KEY `uq_effect_kind` (`payment_id`, `kind`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_configuration` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `kind` VARCHAR(40) NOT NULL,
  `version` BIGINT NULL,
  `published` BOOLEAN NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  KEY `idx_config_kind` (`tenant_id`, `kind`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_number_sequence` (
  `id` VARCHAR(128) NOT NULL PRIMARY KEY,
  `value` BIGINT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_outbox` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `topic` VARCHAR(80) NOT NULL,
  `event_id` VARCHAR(64) NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `payload_json` LONGTEXT NOT NULL,
  `status` VARCHAR(20) NULL,
  `version` BIGINT NULL,
  `attempt` BIGINT NULL,
  `lease_owner` VARCHAR(128) NULL,
  `lease_until` DATETIME(3) NULL,
  `next_attempt_at` DATETIME(3) NULL,
  `last_error` VARCHAR(1000) NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  UNIQUE KEY `idx_exp_outbox_event_id` (`event_id`),
  KEY `idx_exp_outbox_resource_id` (`resource_id`),
  KEY `idx_outbox_due` (`status`, `next_attempt_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_job` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `kind` VARCHAR(40) NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `payload_json` LONGTEXT NOT NULL,
  `status` VARCHAR(20) NULL,
  `version` BIGINT NULL,
  `attempt` BIGINT NULL,
  `lease_owner` VARCHAR(128) NULL,
  `lease_until` DATETIME(3) NULL,
  `next_attempt_at` DATETIME(3) NULL,
  `last_error` VARCHAR(1000) NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL,
  KEY `idx_exp_job_resource_id` (`resource_id`),
  KEY `idx_job_due` (`status`, `next_attempt_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_archive_package` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `package_version` BIGINT NULL,
  `status` VARCHAR(20) NOT NULL,
  `manifest_hash` VARCHAR(64) NULL,
  `manifest_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  UNIQUE KEY `uq_archive_version` (`tenant_id`, `claim_id`, `package_version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_invoice_document` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `owner` VARCHAR(128) NOT NULL,
  `file_id` VARCHAR(128) NULL,
  `status` VARCHAR(30) NOT NULL,
  `version` BIGINT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  `updated_at` DATETIME(3) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_invoice_field_revision` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `invoice_id` VARCHAR(64) NOT NULL,
  `version` BIGINT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  KEY `idx_exp_invoice_field_revision_invoice_id` (`invoice_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_claim_reference` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `claim_id` VARCHAR(64) NOT NULL,
  `project_id` VARCHAR(64) NOT NULL,
  `service_item_id` VARCHAR(64) NULL,
  `item_id` VARCHAR(64) NOT NULL,
  `amount` BIGINT NULL,
  `created_at` DATETIME(3) NULL,
  KEY `idx_reference_project` (`tenant_id`, `project_id`),
  KEY `idx_exp_claim_reference_claim_id` (`claim_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `exp_invoice_verification_attempt` (
  `id` VARCHAR(64) NOT NULL PRIMARY KEY,
  `tenant_id` VARCHAR(64) NOT NULL,
  `invoice_id` VARCHAR(64) NOT NULL,
  `actor` VARCHAR(128) NOT NULL,
  `method` VARCHAR(30) NOT NULL,
  `result` VARCHAR(30) NOT NULL,
  `content_json` LONGTEXT NOT NULL,
  `created_at` DATETIME(3) NULL,
  KEY `idx_exp_invoice_verification_attempt_invoice_id` (`invoice_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
