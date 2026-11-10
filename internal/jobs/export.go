package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

// csvCell 防止用户输入在表格软件中作为公式执行。
// 参数：value为已按范围过滤的业务文本。
// 返回值：可安全写入CSV的文本。
func csvCell(value string) string {
	normalized := strings.TrimLeftFunc(value, unicode.IsSpace)
	if normalized != "" && strings.ContainsRune("=+-@", rune(normalized[0])) {
		return "'" + value
	}
	return value
}

// export 在生成时重新查询可见单据，并将精确导出ID与摘要写入受控任务结果。
// 参数：ctx为任务超时；row为本人导出任务及固定筛选。
// 返回值：生成/授权失败错误；超100张不静默截断。
func (p *Processor) export(ctx context.Context, row Record) error {
	var input struct {
		Actor    domain.Actor      `json:"actor"`
		Filters  map[string]string `json:"filters"`
		ClaimIDs []string          `json:"claim_ids"`
		Result   *struct {
			FileID string `json:"file_id"`
			SHA256 string `json:"sha256"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(row.PayloadJSON), &input) != nil || input.Actor.TenantID != row.TenantID || !input.Actor.Has("expense.archive.export") {
		return errors.New("EXPORT_AUTHORIZATION_INVALID")
	}
	// 本地结果已原子提交后的队列重投只补结束标记；下载仍会独立复核当前授权。
	if input.Result != nil && input.Result.FileID != "" && len(input.Result.SHA256) == 64 {
		return nil
	}
	if p.Personnel == nil {
		return errors.New("EXPORT_AUTHORIZATION_UNAVAILABLE")
	}
	currentActor, err := p.Personnel.CurrentExportActor(ctx, input.Actor)
	if err != nil {
		return err
	}
	input.Actor = currentActor
	claims := []domain.Claim{}
	if len(input.ClaimIDs) > 0 {
		for _, id := range input.ClaimIDs {
			raw, err := p.Service.Query(ctx, input.Actor, "claim", id, nil)
			if err != nil {
				return err
			}
			var claim domain.Claim
			if json.Unmarshal(raw, &claim) != nil {
				return errors.New("EXPORT_SOURCE_INVALID")
			}
			claims = append(claims, claim)
		}
	} else {
		raw, err := p.Service.Query(ctx, input.Actor, "claims", "", input.Filters)
		if err != nil {
			return err
		}
		var page struct {
			Items []domain.Claim `json:"items"`
			Total int            `json:"total"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Total > 100 || len(page.Items) != page.Total {
			return errors.New("EXPORT_LIMIT_EXCEEDED")
		}
		claims = page.Items
	}
	var content bytes.Buffer
	writer := csv.NewWriter(&content)
	if err := writer.Write([]string{"报销单号", "标题", "状态", "应付金额（分）", "币种"}); err != nil {
		return err
	}
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.ID)
		amount, _ := claim.Payable.MarshalJSON()
		if err := writer.Write([]string{csvCell(claim.ClaimNo), csvCell(claim.Title), csvCell(claim.Status), strings.Trim(string(amount), "\""), claim.Currency}); err != nil {
			return err
		}
	}
	writer.Flush()
	if writer.Error() != nil {
		return writer.Error()
	}
	if content.Len() > 1<<20 {
		return errors.New("EXPORT_SIZE_LIMIT")
	}
	hash := sha256.Sum256(content.Bytes())
	// 数据库保存小批受控结果供查询时逐ID重新授权；网关保留受控文件副本。
	fileID, err := p.Files.StoreGenerated(ctx, input.Actor, content.Bytes(), "reimbursement.csv")
	if err != nil {
		return err
	}
	var payload map[string]any
	if json.Unmarshal([]byte(row.PayloadJSON), &payload) != nil {
		return errors.New("EXPORT_SOURCE_INVALID")
	}
	payload["result"] = map[string]any{"filename": "reimbursement.csv", "content_type": "text/csv;charset=utf-8", "content": content.String(), "sha256": hex.EncodeToString(hash[:]), "claim_ids": ids, "file_id": fileID}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return p.commitExport(ctx, row, body)
}

// commitExport 将已生成的受控结果与后台审计原子提交，旧租约不能发布结果。
// 参数：ctx为持久化边界；row为租约凭证；body为有界导出结果。
// 返回值：租约或审计落库失败；已提交结果不重复产生审计。
func (p *Processor) commitExport(ctx context.Context, row Record, body []byte) error {
	return p.withLease(ctx, row, func(tx *gorm.DB) error {
		var current struct{ PayloadJSON string }
		if err := tx.Table("exp_job").Where("id = ? AND tenant_id = ?", row.ID, row.TenantID).Take(&current).Error; err != nil {
			return err
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(current.PayloadJSON), &payload); err != nil {
			return err
		}
		if len(payload["result"]) > 0 && string(payload["result"]) != "null" {
			return nil
		}
		if err := tx.Table("exp_job").Where("id = ? AND tenant_id = ?", row.ID, row.TenantID).Update("payload_json", string(body)).Error; err != nil {
			return err
		}
		return application.AppendWorkerAudit(tx, row.TenantID, row.ResourceID, "export.completed", row.ID, "SUCCESS", "")
	})
}
