-- 不可变付款历史不再承担当前流水占用。仅有完整独立批准及实际反向事实的
-- NO_PAYMENT 可以释放；全额退款及无法证明安全释放的历史一律保留占用。
CREATE TABLE IF NOT EXISTS exp_payment_binding (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  tenant_id VARCHAR(64) NOT NULL,
  method VARCHAR(30) NOT NULL,
  source_account_ref VARCHAR(128) NOT NULL,
  transaction_no VARCHAR(128) NOT NULL,
  payment_id VARCHAR(64) NOT NULL,
  claim_id VARCHAR(64) NOT NULL,
  created_at DATETIME(3) NULL,
  UNIQUE KEY uq_payment_binding_transaction
    (tenant_id, method, source_account_ref, transaction_no),
  UNIQUE KEY uq_payment_binding_payment (payment_id),
  KEY idx_exp_payment_binding_claim_id (claim_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- 先完整回填再移除旧唯一约束；冲突显式阻断，禁止静默丢失历史占用。
INSERT INTO exp_payment_binding
  (id, tenant_id, method, source_account_ref, transaction_no, payment_id, claim_id, created_at)
SELECT p.id, p.tenant_id, p.method, p.source_account_ref, p.transaction_no,
       p.id, p.claim_id, p.created_at
FROM exp_payment p
WHERE NOT EXISTS (
  SELECT 1 FROM exp_payment_binding b WHERE b.payment_id = p.id
) AND NOT EXISTS (
  SELECT 1 FROM exp_payment_adjustment_request r
  JOIN exp_payment_reversal v
    ON v.tenant_id = r.tenant_id AND v.payment_id = r.payment_id AND v.claim_id = r.claim_id
  WHERE r.tenant_id = p.tenant_id AND r.payment_id = p.id AND r.claim_id = p.claim_id
    AND r.status = 'APPROVED'
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.fact_kind')) = 'NO_PAYMENT'
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.status')) = 'APPROVED'
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.payment_id')) = p.id
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.requester_subject_id')) = r.requester
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.approver_subject_id')) <> ''
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.approver_subject_id')) <> r.requester
    AND JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.approver_subject_id')) <> p.actor
    AND JSON_UNQUOTE(JSON_EXTRACT(v.content_json, '$.request.id')) = r.id
    AND JSON_UNQUOTE(JSON_EXTRACT(v.content_json, '$.request.payment_id')) = p.id
    AND JSON_UNQUOTE(JSON_EXTRACT(v.content_json, '$.request.fact_kind')) = 'NO_PAYMENT'
    AND JSON_UNQUOTE(JSON_EXTRACT(v.content_json, '$.request.status')) = 'APPROVED'
    AND JSON_UNQUOTE(JSON_EXTRACT(v.content_json, '$.request.approver_subject_id')) =
        JSON_UNQUOTE(JSON_EXTRACT(r.content_json, '$.approver_subject_id'))
);

-- MySQL DDL 可能在版本记录前成功，受控重试需要容忍索引已经移除。
SET @payment_binding_drop_sql = (
  SELECT IF(COUNT(*) > 0,
    'ALTER TABLE exp_payment DROP INDEX uq_payment_transaction', 'SELECT 1')
  FROM information_schema.statistics
  WHERE table_schema = DATABASE() AND table_name = 'exp_payment'
    AND index_name = 'uq_payment_transaction'
);
PREPARE payment_binding_drop_stmt FROM @payment_binding_drop_sql;
EXECUTE payment_binding_drop_stmt;
DEALLOCATE PREPARE payment_binding_drop_stmt;
