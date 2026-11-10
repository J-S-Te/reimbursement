package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type InvoiceControlInput struct {
	CommandInput
	Reason            string                   `json:"reason"`
	ProposedInvoice   domain.Invoice           `json:"proposed_invoice"`
	Approve           bool                     `json:"approve"`
	CorrectionID      string                   `json:"correction_id"`
	FileIDs           []string                 `json:"file_ids"`
	SourceSelections  []InvoiceSourceSelection `json:"source_selections"`
	RiskKind          string                   `json:"risk_kind"`
	OriginalInvoiceID string                   `json:"original_invoice_id"`
	EvidenceFileIDs   []string                 `json:"evidence_file_ids"`
	Resolution        string                   `json:"resolution"`
}

type invoiceCorrectionContent struct {
	Previous       domain.Invoice `json:"previous"`
	Proposed       domain.Invoice `json:"proposed_invoice"`
	Reason         string         `json:"reason"`
	DecisionReason string         `json:"decision_reason,omitempty"`
}

// preflightInvoiceControl 在本地事务前复核有界附件的真实文件权限，不以解析结果代替人工决定。
// 参数：ctx 为调用边界，db 为数据库，cc 为票据控制命令，payload 为不可变请求。
// 返回值：原因、附件、归属或外部文件服务错误。
func (s *Service) preflightInvoiceControl(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	var input InvoiceControlInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return e
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return domain.Fail("REASON_REQUIRED", "票据控制必须填写明确原因", 422)
	}
	var ids []string
	switch cc.operation {
	case "invoice.correction.request":
		ids = input.ProposedInvoice.FileIDs
	case "invoice.source.append":
		ids = input.FileIDs
	case "invoice.risk.record":
		ids = input.EvidenceFileIDs
	default:
		return nil
	}
	if len(ids) == 0 || len(ids) > 20 || len(input.SourceSelections) > 20 {
		return domain.Fail("FILE_REFERENCE_LIMIT", "票据控制须关联1至20份授权原件", 422)
	}
	for _, id := range ids {
		if _, e := invoiceControlDocument(db, cc.actor, id); e != nil {
			return e
		}
	}
	if s.deps.Files == nil {
		return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务未配置", 503)
	}
	return s.deps.Files.Validate(ctx, cc.actor, ids, "reimbursement.invoice.original")
}

// invoiceControl 分发独立票据更正、来源追加、风险复核和申请人重确认。
// 参数：tx 为统一命令事务，cc 为当前授权身份，payload 为请求。
// 返回值：冻结控制回执、所属票据或单据标识及错误。
func (s *Service) invoiceControl(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input InvoiceControlInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return nil, "", domain.Fail("REASON_REQUIRED", "请填写明确原因", 422)
	}
	switch cc.operation {
	case "invoice.correction.request":
		return requestInvoiceCorrection(tx, cc, input)
	case "invoice.correction.decide":
		return decideInvoiceCorrection(tx, cc, input)
	case "claim.invoice.reconfirm":
		return reconfirmInvoiceCorrection(tx, cc, input)
	case "invoice.source.append":
		return appendControlledInvoiceSources(tx, cc, input)
	case "invoice.risk.record":
		return recordInvoiceRisk(tx, cc, input)
	case "invoice.risk.close":
		return closeInvoiceRisk(tx, cc, input)
	}
	return nil, "", domain.Fail("OPERATION_NOT_SUPPORTED", "不支持的票据控制动作", 422)
}

// lockControlledInvoice 先锁归属单据再锁票据，和提交、付款沿用一致锁顺序。
// 参数：tx 为事务，tenant/id 为可信票据定位。
// 返回值：当前票据及可选归属单据；关联并发变更要求重新提交命令。
func lockControlledInvoice(tx *gorm.DB, tenant, id string) (store.Invoice, *domain.Claim, error) {
	var row store.Invoice
	if e := tx.Where("tenant_id = ? AND id = ?", tenant, id).First(&row).Error; e != nil {
		return row, nil, domain.Fail("RESOURCE_NOT_FOUND", "票据不存在或无权访问", 404)
	}
	claimID := row.ReservationClaimID
	var c *domain.Claim
	if claimID != "" {
		claim, _, e := loadClaim(tx, tenant, claimID, true)
		if e != nil {
			return row, nil, e
		}
		c = &claim
	}
	row = store.Invoice{}
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenant, id).First(&row).Error; e != nil {
		return row, nil, e
	}
	if row.ReservationClaimID != claimID {
		return row, nil, domain.Fail("VERSION_CONFLICT", "票据归属已改变，请刷新后重试", 409)
	}
	return row, c, nil
}

// invoiceControlDocument 只接受本租户本人已完成上传的真实票据原件。
// 参数：db 为数据库，a 为操作身份，fileID 为文件引用。
// 返回值：固定文档关系；跨租户、未完成或用途错误时拒绝。
func invoiceControlDocument(db *gorm.DB, a domain.Actor, fileID string) (store.Document, error) {
	var doc store.Document
	e := db.Where("tenant_id = ? AND file_id = ? AND owner = ?", a.TenantID, fileID, a.SubjectID).First(&doc).Error
	if e != nil {
		return doc, domain.Fail("INVOICE_SOURCE_INVALID", "原件不存在或不属于当前主体", 422)
	}
	var metadata struct {
		Purpose string `json:"purpose"`
	}
	if e = json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return doc, e
	}
	if (doc.Status != "READY" && doc.Status != "PARSED") || metadata.Purpose != "reimbursement.invoice.original" {
		return doc, domain.Fail("INVOICE_SOURCE_INVALID", "仅可引用已完成上传的票据原件", 422)
	}
	return doc, nil
}

// requireInvoiceControlReview 对独立财务决定同时检查更正能力、财务审核和当前单据范围。
// 参数：tx 为数据库，a 为审核人，row 为票据，c 为当前关联单据。
// 返回值：能力、范围、归属或自审错误。
func requireInvoiceControlReview(tx *gorm.DB, a domain.Actor, row store.Invoice, c *domain.Claim) error {
	if e := require(a, "expense.invoice.correct"); e != nil {
		return e
	}
	if c == nil {
		return domain.Fail("RESOURCE_NOT_FOUND", "票据尚无可核实的财务归属范围", 404)
	}
	if a.SubjectID == row.Owner || a.SubjectID == c.Applicant {
		return domain.Fail("SELF_FINANCIAL_ACTION_FORBIDDEN", "票据所有人和申请人不能自行批准票据更正", 403)
	}
	return requireFinancial(tx, a, *c, "expense.finance.review")
}

// invoiceControlRecords 按票据有界读取风险和更正影响，禁止静默截断控制状态。
// 参数：tx 为数据库，tenant/invoiceID 为票据边界。
// 返回值：控制影响与风险案件或规模错误。
func invoiceControlRecords(tx *gorm.DB, tenant, invoiceID string) ([]store.InvoiceControlImpact, []store.InvoiceRisk, error) {
	var impacts []store.InvoiceControlImpact
	var risks []store.InvoiceRisk
	if e := tx.Where("tenant_id = ? AND invoice_id = ?", tenant, invoiceID).Limit(1001).Find(&impacts).Error; e != nil {
		return nil, nil, e
	}
	if e := tx.Where("tenant_id = ? AND (invoice_id = ? OR original_invoice_id = ?)", tenant, invoiceID, invoiceID).Limit(1001).Find(&risks).Error; e != nil {
		return nil, nil, e
	}
	if len(impacts) > 1000 || len(risks) > 1000 {
		return nil, nil, domain.Fail("INVOICE_CONTROL_LIMIT", "票据控制历史超过受控上限", 422)
	}
	return impacts, risks, nil
}

// VerifyInvoiceControls 在持有单据锁的提交或付款事务中检查票据重大更正和风险闭环。
// 参数：tx 为业务事务，c 为当前单据，phase 为SUBMIT或PAYMENT。
// 返回值：仍待核实、未重新确认或未重审时拒绝，不修改金融事实。
func VerifyInvoiceControls(tx *gorm.DB, c domain.Claim, phase string) error {
	for _, id := range invoiceIDs(c) {
		impacts, risks, e := invoiceControlRecords(tx, c.TenantID, id)
		if e != nil {
			return e
		}
		for _, risk := range risks {
			if risk.Status != "CLOSED" || risk.Resolution != "DISMISSED" {
				return domain.Fail("INVOICE_RISK_REVIEW_REQUIRED", "关联票据存在尚未解除的红字、作废、退款或财务复核风险", 409)
			}
		}
		for _, impact := range impacts {
			if impact.ClaimID != c.ID || impact.Status == "REJECTED" || impact.Status == "RESOLVED" {
				continue
			}
			if impact.Status != "RECONFIRMED" || (phase == "PAYMENT" && c.CurrentRound <= impact.OriginalRound) {
				return domain.Fail("INVOICE_RECONFIRMATION_REQUIRED", "票据重大更正须由申请人重新确认并完成新轮次审批", 409)
			}
			var row store.Invoice
			if e = tx.Where("tenant_id = ? AND id = ?", c.TenantID, id).First(&row).Error; e != nil {
				return e
			}
			// reserveInvoices会递增行版本；确认后只有本单合法预留不改变票面。
			var content domain.Invoice
			if e = json.Unmarshal([]byte(row.ContentJSON), &content); e != nil {
				return e
			}
			if content.Version != impact.InvoiceVersion {
				return domain.Fail("INVOICE_RECONFIRMATION_REQUIRED", "票据内容再次变化，请重新确认", 409)
			}
		}
	}
	return nil
}

// invoiceControlNotFound 将缺少控制案件转换为无泄漏的业务错误。
// 参数：e 为持久化查询错误。
// 返回值：案件不存在或原始存储错误。
func invoiceControlNotFound(e error) error {
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return domain.Fail("RESOURCE_NOT_FOUND", "票据控制案件不存在或无权访问", 404)
	}
	return e
}
