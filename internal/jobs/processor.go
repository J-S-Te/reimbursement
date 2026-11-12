package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	core "github.com/J-S-Te/license-core"
	"github.com/oklog/ulid/v2"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"github.com/j-s-te/reimbursement/internal/invoiceparser"
	"gorm.io/gorm"
)

type LicenseGate interface {
	Check(context.Context, core.Operation) error
}

type Processor struct {
	Personnel                     *integration.Personnel
	License                       LicenseGate
	DB                            *gorm.DB
	Service                       *application.Service
	Files                         *integration.Files
	Audit, Notifications, Project *integration.Client
	Environment                   string
	Parser                        *invoiceparser.Parser
}

// Outbox 将已冻结业务事件转换为平台DTO并稳定重投，不重做财务命令。
// 参数：ctx为投递超时；row为已领取的本地outbox。
// 返回值：远端受理结果；失败交给租约队列退避重试。
func (p *Processor) Outbox(ctx context.Context, row Record) error {
	if p.License == nil {
		return errors.New("LICENSE_UNAVAILABLE")
	}
	if err := p.License.Check(ctx, outboxLicenseOperation(row.Topic)); err != nil {
		return errors.New("LICENSE_UNAVAILABLE")
	}
	var data map[string]any
	if json.Unmarshal([]byte(row.PayloadJSON), &data) != nil {
		return errors.New("INVALID_OUTBOX_PAYLOAD")
	}
	var err error
	var accepted json.RawMessage
	switch row.Topic {
	case "project", "project_spend":
		if err = integration.RequireProjectTenant(ctx, p.Project, row.TenantID); err != nil {
			return err
		}
		accepted, err = p.Project.Call(ctx, "POST", "/internal/v1/reimbursement/spend-snapshots", json.RawMessage(row.PayloadJSON))
		if err == nil {
			var receipt struct {
				Status           string `json:"status"`
				ReceivedRevision uint64 `json:"received_revision"`
				AppliedRevision  uint64 `json:"applied_revision"`
				SnapshotHash     string `json:"snapshot_hash"`
			}
			var snap domain.SpendSnapshot
			if json.Unmarshal(accepted, &receipt) != nil || json.Unmarshal([]byte(row.PayloadJSON), &snap) != nil || receipt.ReceivedRevision != snap.ProjectionRevision || receipt.SnapshotHash != snap.SnapshotHash || receipt.AppliedRevision < snap.ProjectionRevision || (receipt.Status != "APPLIED" && receipt.Status != "DUPLICATE" && receipt.Status != "SUPERSEDED") {
				return errors.New("INVALID_PROJECT_RECEIPT")
			}
		}
	case "project_rebuild":
		if err = integration.RequireProjectTenant(ctx, p.Project, row.TenantID); err != nil {
			return err
		}
		var input struct {
			ClaimID string `json:"claim_id"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal([]byte(row.PayloadJSON), &input) != nil || input.ClaimID != row.ResourceID || strings.TrimSpace(input.Reason) == "" {
			return errors.New("INVALID_REBUILD_PAYLOAD")
		}
		accepted, err = p.Project.Call(ctx, "POST", "/internal/v1/reimbursement/spend-snapshots/rebuild", input)
		if err == nil {
			var result struct {
				Rebuilt bool `json:"rebuilt"`
			}
			if json.Unmarshal(accepted, &result) != nil || !result.Rebuilt {
				return errors.New("INVALID_REBUILD_RECEIPT")
			}
		}
	case "audit":
		trace := sha256.Sum256([]byte(row.EventID))
		traceID := hex.EncodeToString(trace[:16])
		requestID := auditRequestID(data, row.EventID)
		correlationID := auditCorrelationID(data, row.EventID)
		payload := map[string]any{"event_id": auditEventID(row.EventID), "occurred_at": row.CreatedAt.UTC().Format(time.RFC3339Nano), "application_code": "reimbursement", "environment_code": p.Environment, "actor_id": data["actor_subject_id"], "actor_type": auditActorType(data), "action": data["action"], "resource_type": "expense", "resource_id": row.ResourceID, "result": auditResult(data), "risk_level": "MEDIUM", "request_id": requestID, "trace_id": traceID, "correlation_id": correlationID}
		// 单事件摄取要求审计体中的 request/trace/correlation 与传输头精确一致；
		// 不携带这些头会被平台按 422 拒绝，导致 outbox 长期重试并丢掉审计证据。
		spanID := hex.EncodeToString(trace[16:24])
		if spanID == "0000000000000000" {
			spanID = "0000000000000001"
		}
		headers := map[string]string{
			"X-Request-ID":     requestID,
			"X-Correlation-ID": correlationID,
			"traceparent":      "00-" + traceID + "-" + spanID + "-01",
		}
		_, err = p.Audit.CallWithHeaders(ctx, "POST", "/api/v1/audit/events", payload, headers)
	case "notification":
		if data["kind"] == "APPROVAL_REMINDER" {
			deliverable, checkErr := p.ApprovalReminderDeliverable(ctx, row, data)
			if checkErr != nil {
				return checkErr
			}
			if !deliverable {
				return nil
			}
		}
		payload, payloadErr := notificationPayload(row, data)
		if payloadErr != nil {
			return payloadErr
		}
		if p.Notifications == nil {
			return errors.New("NOTIFICATION_UNAVAILABLE")
		}
		accepted, err = p.Notifications.Call(ctx, "POST", "/api/v1/notifications/events", payload)
		if err == nil {
			err = notificationAcceptance(accepted, payload["event_id"].(string))
		}
	default:
		return errors.New("OUTBOX_TOPIC_UNSUPPORTED")
	}
	if err == nil && len(accepted) > 0 {
		return p.persistAcceptance(ctx, row, accepted)
	}
	return err
}

// Job 处理持久化解析或归档任务，任务失败不回滚已登记付款。
// 参数：ctx为任务超时；row为有租约的任务。
// 返回值：处理结果；不支持任务明确失败。
func (p *Processor) Job(ctx context.Context, row Record) error {
	operation := core.MUTATE_BUSINESS
	if row.Kind == "ARCHIVE" || row.Kind == "EXPORT" || row.Kind == "ARCHIVE_PACKAGE" {
		operation = core.EXPORT_HISTORY
	}
	if p.License == nil {
		return errors.New("LICENSE_UNAVAILABLE")
	}
	if err := p.License.Check(ctx, operation); err != nil {
		return errors.New("LICENSE_UNAVAILABLE")
	}
	switch row.Kind {
	case "INVOICE_PARSE":
		return p.parse(ctx, row)
	case "ARCHIVE":
		return p.archive(ctx, row)
	case "EXPORT":
		return p.export(ctx, row)
	case "ARCHIVE_PACKAGE":
		return p.archivePackage(ctx, row)
	default:
		return errors.New("JOB_KIND_UNSUPPORTED")
	}
}

// parse 提取原件候选，原始来源留档，人工确认与查验状态保持独立。
// 参数：ctx为解析超时；row绑定租户及文档。
// 返回值：候选持久化或资源解析错误。
func (p *Processor) parse(ctx context.Context, row Record) (err error) {
	lease := application.ParseLease{JobID: row.ID, Owner: row.LeaseOwner, Version: row.Version}
	if committed, e := p.Service.ParseAlreadyCommitted(ctx, row.TenantID, row.ResourceID, row.ID); e != nil {
		return e
	} else if committed {
		return nil
	}
	if err = p.Service.StartParseAttempt(ctx, row.TenantID, row.ResourceID, lease); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = p.recordParseFailure(ctx, row, lease, err)
		}
	}()
	raw, err := p.Files.ReadDocument(ctx, row.TenantID, row.ResourceID)
	if err != nil {
		return err
	}
	var doc store.Document
	if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", row.TenantID, row.ResourceID).First(&doc).Error; err != nil {
		return err
	}
	var metadata struct {
		MediaType string `json:"media_type"`
	}
	if err = json.Unmarshal([]byte(doc.ContentJSON), &metadata); err != nil {
		return err
	}
	result, err := invoiceparser.Isolated(ctx, raw, metadata.MediaType)
	if err != nil {
		return err
	}
	candidates := make([]domain.Invoice, 0, len(result.Documents))
	indexes := []int{}
	for index, c := range result.Documents {
		if c.GroupKind == "AMBIGUOUS_MERGE_OR_CONTINUATION" || c.GroupKind == "OFD_MULTIPLE_DOCUMENTS" {
			continue
		}
		amount, amountErr := strconv.ParseInt(c.GrossMinor, 10, 64)
		if amountErr != nil || amount <= 0 {
			continue
		}
		indexes = append(indexes, index)
		candidates = append(candidates, domain.Invoice{InvoiceType: c.InvoiceType, InvoiceNumber: c.Number, InvoiceCode: c.Code, InvoiceDate: c.Date, BuyerTaxID: c.BuyerTaxID, SellerTaxID: c.SellerTaxID, Total: domain.Money(amount), FileIDs: []string{doc.FileID}, ReviewStatus: "PENDING", VerificationStatus: "NOT_CHECKED"})
	}
	lease.CandidateIndexes = indexes
	original, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return p.Service.CompleteParseLeased(ctx, row.TenantID, row.ResourceID, candidates, parseIssues(result), original, lease)
}

// auditRequestID 透传首次业务请求追踪号，后台旧事件才使用稳定事件ID回退。
// 参数：payload为已冻结审计载荷；fallback为稳定事件ID。
// 返回值：不含控制字符的有界追踪号。
func auditRequestID(payload map[string]any, fallback string) string {
	if value, ok := payload["request_id"].(string); ok && value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n") {
		return value
	}
	return fallback
}

// auditEventID 从持久化 outbox 事件 ID 派生稳定 ULID。
// 参数：raw 为 outbox 事件 ID。
// 返回值：审计平台接受的 26 位 ULID；重试时保持一致以支持幂等。
func auditEventID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	var id ulid.ULID
	copy(id[:], sum[:16])
	return id.String()
}

// auditCorrelationID 返回与审计请求头一致的操作级关联标识。
// 参数：payload为事务内载荷，fallback为事件ID。
// 返回值：非空且无控制字符的命令标识；缺失时退回事件ID。
func auditCorrelationID(payload map[string]any, fallback string) string {
	if value, ok := payload["command_id"].(string); ok {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n") {
			return value
		}
	}
	return fallback
}

// auditActorType 仅允许冻结系统事件显式声明系统主体。
// 参数：payload为事务内创建的可靠审计载荷。
// 返回值：SYSTEM或默认USER，不透传任意主体类型。
func auditActorType(payload map[string]any) string {
	if payload["actor_type"] == "SYSTEM" {
		return "SYSTEM"
	}
	return "USER"
}

// auditResult 保留后台失败事实，其余旧业务事件维持成功结果。
// 参数：payload为事务内创建的可靠审计载荷。
// 返回值：平台支持的SUCCESS或FAILURE。
func auditResult(payload map[string]any) string {
	if payload["result"] == "FAILURE" {
		return "FAILURE"
	}
	return "SUCCESS"
}

// parseIssues 合并总体与候选诊断，便于人工识别金额缺失或无法安全拆票。
// 参数：result为受控解析输出。
// 返回值：按出现顺序去重的诊断，不将候选问题静默丢弃。
func parseIssues(result invoiceparser.Result) []string {
	issues := []string{}
	seen := map[string]bool{}
	lists := [][]string{result.Warnings}
	for _, candidate := range result.Documents {
		lists = append(lists, candidate.Issues)
	}
	for _, list := range lists {
		for _, issue := range list {
			if !seen[issue] {
				issues = append(issues, issue)
				seen[issue] = true
			}
		}
	}
	return issues
}

// recordParseFailure 保证失败运行及审计未持久化时仍保留可恢复任务。
// 参数：ctx为原任务边界；row/lease绑定本次尝试；cause为原解析失败。
// 返回值：原失败或稳定持久化故障码，避免永久错误掩盖审计落库失败。
func (p *Processor) recordParseFailure(ctx context.Context, row Record, lease application.ParseLease, cause error) error {
	failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	code := cause.Error()
	if len(code) > 100 || strings.ContainsAny(code, " \n\r") {
		code = "PARSER_FAILED"
	}
	if err := p.Service.FailParseAttempt(failureCtx, row.TenantID, row.ResourceID, lease, code); err != nil {
		return errors.New("PARSE_FAILURE_PERSISTENCE_UNAVAILABLE")
	}
	return cause
}
