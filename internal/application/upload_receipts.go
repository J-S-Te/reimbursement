package application

import (
	"encoding/json"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// completedDocumentReceipt 返回已成功完成上传的本地生命周期回执，回放不再依赖网关。
// 参数：row 为已验证当前归属及用途能力的文档。
// 返回值：冻结回执、完成标记及存储格式错误；旧数据仅返回已记录的完成事实。
func completedDocumentReceipt(row store.Document) (json.RawMessage, bool, error) {
	var metadata map[string]json.RawMessage
	if e := json.Unmarshal([]byte(row.ContentJSON), &metadata); e != nil {
		return nil, false, e
	}
	if raw, exists := metadata["complete_receipt"]; exists && string(raw) != "null" {
		return raw, true, nil
	}
	if _, exists := metadata["completion"]; !exists || row.FileID == "" || row.Status == "UPLOAD_PENDING" {
		return nil, false, nil
	}
	var jobID string
	_ = json.Unmarshal(metadata["parse_job_id"], &jobID)
	raw, e := json.Marshal(map[string]any{"document_id": row.ID, "file_id": row.FileID, "status": "READY", "parse_job_id": jobID, "parse_run_id": jobID, "version": row.Version})
	return raw, true, e
}
