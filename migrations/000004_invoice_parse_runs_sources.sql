CREATE TABLE IF NOT EXISTS exp_invoice_parse_run (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 document_id VARCHAR(64) NOT NULL,
 job_id VARCHAR(64) NOT NULL,
 attempt INT NOT NULL,
 parser_version VARCHAR(128) NOT NULL,
 rules_version VARCHAR(64) NOT NULL,
 input_sha256 VARCHAR(64) NOT NULL,
 status VARCHAR(30) NOT NULL,
 result_hash VARCHAR(64) NOT NULL,
 result_json LONGTEXT NOT NULL,
 failure_code VARCHAR(100) NOT NULL,
 created_at DATETIME(3) NOT NULL,
 completed_at DATETIME(3) NULL,
 UNIQUE KEY uq_parse_attempt(tenant_id,job_id,attempt),
 KEY idx_parse_document(tenant_id,document_id,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS exp_invoice_source (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 invoice_id VARCHAR(64) NOT NULL,
 document_id VARCHAR(64) NOT NULL,
 parse_run_id VARCHAR(64) NOT NULL,
 candidate_index INT NOT NULL,
 sources_hash VARCHAR(64) NOT NULL,
 sources_json LONGTEXT NOT NULL,
 created_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_invoice_source(tenant_id,invoice_id,document_id,parse_run_id,candidate_index),
 KEY idx_source_document(tenant_id,document_id,invoice_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
