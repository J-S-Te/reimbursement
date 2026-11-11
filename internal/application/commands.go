package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CommandInput struct {
	CommandID       string `json:"command_id"`
	ExpectedVersion int64  `json:"expected_version"`
}
type commandContext struct {
	appointment       *draftAppointmentProof
	finance           *financeRecheckProof
	recovery          *approvalRecoveryProof
	business          *businessActionProof
	transferTenantAll bool
	submission        *submissionProof
	transfer          *domain.TransferQualification
	actor             domain.Actor
	operation         string
	resourceID        string
	input             CommandInput
	hash              string
	refs              []domain.ProjectReference
}

// newID 生成与业务编号分离的随机内部标识。
// 参数：无。
// 返回值：随机标识，系统熵源异常时返回错误。
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// digest 计算稳定请求与内容摘要。
// 参数：v 为可 JSON 编码的业务数据。
// 返回值：SHA256摘要或编码错误。
func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// encode 将受控领域对象编码为数据库内容。
// 参数：v 为不包含凭据的业务对象。
// 返回值：JSON字符串或编码错误。
func encode(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }

// require 校验可信身份与精确动作权限。
// 参数：a 为当前身份，permission 为业务权限。
// 返回值：身份或权限错误。
func require(a domain.Actor, permission string) error {
	if a.TenantID == "" || a.SubjectID == "" {
		return domain.Fail("SESSION_EXPIRED", "请重新登录报销系统", 401)
	}
	if !a.Has(permission) {
		return domain.Fail("FORBIDDEN", "没有此操作权限", 403)
	}
	return nil
}

// loadClaim 按租户读取单据，并可锁定聚合根。
// 参数：db 为当前事务，tenant/id 为对象边界，lock 表示写锁。
// 返回值：单据与持久化行或不存在错误。
func loadClaim(db *gorm.DB, tenant, id string, lock bool) (domain.Claim, store.Claim, error) {
	var row store.Claim
	q := db.Where("tenant_id = ? AND id = ?", tenant, id)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := q.First(&row).Error
	var c domain.Claim
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c, row, domain.Fail("RESOURCE_NOT_FOUND", "报销单不存在或无权访问", 404)
	}
	if err == nil {
		err = json.Unmarshal([]byte(row.ContentJSON), &c)
	}
	return c, row, err
}

// saveClaim 以版本比较更新聚合，防止并发命令覆盖。
// 参数：tx 为事务，c 为新状态，previous 为修改前版本。
// 返回值：持久化错误或版本冲突。
func saveClaim(tx *gorm.DB, c *domain.Claim, previous int64) error {
	c.UpdatedAt = time.Now().UTC()
	raw, err := encode(c)
	if err != nil {
		return err
	}
	var active *string
	if c.ActivePaymentID != "" {
		active = &c.ActivePaymentID
	}
	r := tx.Model(&store.Claim{}).Where("tenant_id = ? AND id = ? AND version = ?", c.TenantID, c.ID, previous).Updates(map[string]any{"org_id": c.OrgID, "status": c.Status, "version": c.Version, "active_payment_id": active, "content_json": raw, "updated_at": c.UpdatedAt})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return domain.Fail("VERSION_CONFLICT", "单据已改变，请刷新后再操作", 409)
	}
	return nil
}

// verifyVersion 检查命令的聚合版本。
// 参数：actual 为当前版本，expected 为客户端看到的版本。
// 返回值：版本不一致错误。
func verifyVersion(actual, expected int64) error {
	if actual != expected {
		return domain.Fail("VERSION_CONFLICT", "数据已改变，请刷新后再操作", 409)
	}
	return nil
}

// replay 优先读取成功命令，避免旧版本及远端故障影响重试。
// 参数：db 为数据库，c 为当前身份和请求摘要。
// 返回值：已冻结结果、命中标记与冲突错误。
func (s *Service) replay(db *gorm.DB, c commandContext) (json.RawMessage, bool, error) {
	var row store.Command
	err := db.Where("tenant_id = ? AND actor = ? AND operation = ? AND command_id = ?", c.actor.TenantID, c.actor.SubjectID, c.operation, c.input.CommandID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if row.RequestHash != c.hash {
		return nil, true, domain.Fail("COMMAND_PAYLOAD_CONFLICT", "此命令标识已用于不同请求，请勿修改重试内容", 409)
	}
	if err = s.resultVisible(db, c.actor, row); err != nil {
		return nil, true, err
	}
	return json.RawMessage(row.ResultJSON), true, nil
}

// resultVisible 在重放时重新检查当前结果可见性。
// 参数：db 为数据库，actor 为当前身份，row 为冻结命令。
// 返回值：权限已撤销时返回拒绝。
func (s *Service) resultVisible(db *gorm.DB, a domain.Actor, row store.Command) error {
	if row.Operation == "archive.borrow.request" || row.Operation == "archive.borrow.decide" {
		borrow, e := loadArchiveBorrow(db, a.TenantID, row.ResourceID, false)
		if e != nil {
			return e
		}
		if row.Operation == "archive.borrow.decide" {
			return s.requireBorrowReviewer(db, a, borrow)
		}
		return s.authorizeBorrow(db, a, borrow)
	}
	if row.Operation == "archive.package.create" {
		return s.requireArchivePackage(db, a, row.ResourceID)
	}
	if row.Operation == "export.create" {
		return require(a, "expense.archive.export")
	}
	if row.Operation == "config.publish" || row.Operation == "config.retire" || row.Operation == "config.replace" {
		return require(a, "expense.policy.publish")
	}
	if strings.HasPrefix(row.Operation, "config.") {
		return require(a, "expense.config.manage")
	}
	if strings.HasPrefix(row.Operation, "payment.exception.") {
		c, _, e := loadClaim(db, a.TenantID, row.ResourceID, false)
		if e != nil {
			return e
		}
		return requireExceptionRead(db, a, c)
	}
	if row.Operation == "approval.recover" {
		c, _, e := loadClaim(db, a.TenantID, row.ResourceID, false)
		if e != nil {
			return e
		}
		return requireApprovalRecoveryScope(db, a, c)
	}
	if strings.HasPrefix(row.Operation, "invoice.") {
		return require(a, "expense.invoice.read")
	}
	if strings.HasPrefix(row.Operation, "claim.") || strings.HasPrefix(row.Operation, "payment.") || strings.HasPrefix(row.Operation, "reversal.") || row.Operation == "approval.action" || row.Operation == "archive.retry" {
		c, _, e := loadClaim(db, a.TenantID, row.ResourceID, false)
		if e != nil {
			return e
		}
		return s.canRead(db, a, c)
	}
	return require(a, "expense.claim.read")
}

// Execute 执行带去重、远端预检和本地原子账本的业务命令。
// 参数：ctx 为上下文，actor 为可信身份，operation 为动作，resourceID 为对象，payload 为请求。
// 返回值：冻结响应；失败不会保存成功结果。
func (s *Service) Execute(ctx context.Context, actor domain.Actor, operation, resourceID string, payload json.RawMessage) (json.RawMessage, error) {
	if actor.TenantID == "" || actor.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	var input CommandInput
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, domain.Fail("INVALID_REQUEST", "请求格式不正确", 422)
	}
	if strings.TrimSpace(input.CommandID) == "" || len(input.CommandID) > 128 {
		return nil, domain.Fail("COMMAND_ID_REQUIRED", "请提供有效命令标识", 422)
	}
	var canonical any
	if err := json.Unmarshal(payload, &canonical); err != nil {
		return nil, err
	}
	hash, err := digest([]any{operation, resourceID, canonical})
	if err != nil {
		return nil, err
	}
	cc := commandContext{actor: actor, operation: operation, resourceID: resourceID, input: input, hash: hash}
	db := s.db.WithContext(ctx)
	if result, found, e := s.replay(db, cc); found || e != nil {
		return result, e
	}
	if err = s.preflight(ctx, db, &cc, payload); err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = localTransaction(ctx, db, func(tx *gorm.DB) error {
		if r, found, e := s.replay(tx, cc); found || e != nil {
			result = r
			return e
		}
		value, resource, e := s.dispatch(tx, &cc, payload)
		if e != nil {
			return e
		}
		raw, e := encode(value)
		if e != nil {
			return e
		}
		id, e := newID()
		if e != nil {
			return e
		}
		row := store.Command{ID: id, TenantID: actor.TenantID, Actor: actor.SubjectID, Operation: operation, CommandID: input.CommandID, RequestHash: hash, ResourceID: resource, ResultJSON: raw, CreatedAt: time.Now().UTC()}
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		if e = appendOutbox(tx, actor.TenantID, "audit", resource, map[string]any{"actor_subject_id": actor.SubjectID, "request_id": actor.RequestID, "action": operation, "resource_id": resource, "command_id": input.CommandID, "result": "SUCCESS"}); e != nil {
			return e
		}
		result = json.RawMessage(raw)
		return nil
	})
	if err != nil {
		if r, found, e := s.replay(db, cc); found {
			return r, e
		}
	}
	return result, err
}

// appendOutbox 将可靠投递记录与业务事实写入同一事务。
// 参数：tx 为事务，tenant 为租户，topic 为投递器，resource 为对象，payload 为冻结载荷。
// 返回值：创建失败错误。
func appendOutbox(tx *gorm.DB, tenant, topic, resource string, payload any) error {
	id, err := newID()
	if err != nil {
		return err
	}
	raw, err := encode(payload)
	if err != nil {
		return err
	}
	return tx.Create(&store.Outbox{ID: id, EventID: id, TenantID: tenant, Topic: topic, ResourceID: resource, PayloadJSON: raw, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}).Error
}

// dispatch 在唯一事务中路由具体业务命令。
// 参数：tx 为当前事务，cc 为已验证命令上下文，payload 为不可变请求。
// 返回值：业务结果、结果所属对象与错误。
func (s *Service) dispatch(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	switch cc.operation {
	case "claim.create", "claim.update":
		return s.editClaim(tx, cc, payload)
	case "claim.reopen", "claim.approved_void":
		return s.controlledClaimChange(tx, cc, payload)
	case "payment.exception.record":
		return s.recordPaymentException(tx, cc, payload)
	case "payment.exception.review":
		return s.reviewPaymentException(tx, cc, payload)
	case "claim.submit":
		return s.submit(tx, cc)
	case "claim.withdraw", "claim.void":
		return s.closeClaim(tx, cc)
	case "invoice.create", "invoice.confirm":
		return s.editInvoice(tx, cc, payload)
	case "invoice.reparse":
		return s.queueReparse(tx, cc)
	case "invoice.verify":
		return s.verifyInvoice(tx, cc, payload)
	case "invoice.correction.request", "invoice.correction.decide", "claim.invoice.reconfirm", "invoice.source.append", "invoice.risk.record", "invoice.risk.close":
		return s.invoiceControl(tx, cc, payload)
	case "approval.action":
		return s.approval(tx, cc, payload)
	case "approval.recover":
		return s.recoverApproval(tx, cc, payload)
	case "payment.record":
		return s.recordPayment(tx, cc, payload)
	case "reversal.request", "reversal.decide":
		return s.reversePayment(tx, cc, payload)
	case "config.retire", "config.replace":
		return s.lifecyclePolicy(tx, cc, payload)
	case "config.save", "config.publish":
		return s.configure(tx, cc, payload)
	case "archive.package.create":
		return s.queueArchivePackage(tx, cc)
	case "archive.retry":
		return s.archiveRetry(tx, cc)
	default:
		return nil, "", domain.Fail("OPERATION_NOT_SUPPORTED", fmt.Sprintf("不支持的业务命令: %s", cc.operation), 422)
	}
}
