-- 下载包按创建时间分批清理；补建时间索引，避免保留期清理扫描整张 LONGBLOB 表。
-- MySQL DDL 可能在版本记录前成功，受控重试需要容忍索引已经存在。
SET @archive_download_created_sql = (
  SELECT IF(COUNT(*) = 0,
    'CREATE INDEX idx_archive_download_created ON exp_archive_download (created_at)', 'SELECT 1')
  FROM information_schema.statistics
  WHERE table_schema = DATABASE()
    AND table_name = 'exp_archive_download'
    AND index_name = 'idx_archive_download_created'
);
PREPARE archive_download_created_stmt FROM @archive_download_created_sql;
EXECUTE archive_download_created_stmt;
DEALLOCATE PREPARE archive_download_created_stmt;
