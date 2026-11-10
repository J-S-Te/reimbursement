CREATE TABLE IF NOT EXISTS exp_archive_download (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 job_id VARCHAR(64) NOT NULL,
 claim_id VARCHAR(64) NOT NULL,
 archive_id VARCHAR(64) NOT NULL,
 manifest_hash VARCHAR(64) NOT NULL,
 sha256 VARCHAR(64) NOT NULL,
 content LONGBLOB NOT NULL,
 created_at DATETIME(3) NULL,
 UNIQUE KEY uq_archive_download_job (job_id),
 KEY idx_archive_download_claim (tenant_id,claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
