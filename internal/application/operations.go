package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type RecoveryInput struct {
	CommandInput
	Reason string `json:"reason"`
}

// RecoveryModels 返回可靠恢复所需的独立持久模型。
// 参数：无。
// 返回值：用于测试与迁移校验的模型集合。
func RecoveryModels() []any {
	return []any{&store.RecoveryCommand{}, &store.DeliveryReceipt{}, &store.ReconciliationCursor{}, &store.ReconciliationFinding{}}
}

// operationalClaims 仅构造当前租户且已发布组织范围中的单据查询。
// 参数：db 为数据库，a 为具备集成管理能力的主体。
// 返回值：范围过滤查询与配置读取错误。
func operationalClaims(db *gorm.DB, a domain.Actor) (*gorm.DB, error) {
	if e := require(a, "expense.integration.manage"); e != nil {
		return nil, e
	}
	orgs, all, e := scopeOrganizations(db, a)
	if e != nil {
		return nil, e
	}
	q := db.Model(&store.Claim{}).Where("tenant_id = ?", a.TenantID)
	if !all {
		if len(orgs) == 0 {
			q = q.Where("1 = 0")
		} else {
			q = q.Where("org_id IN ?", orgs)
		}
	}
	return q, nil
}

// recoveryResource 校验队列对象当前归属，解析任务按人员档案组织授权。
// 参数：tx 为数据库，a 为主体，resource 为单据或文档标识。
// 返回值：无范围时隐藏对象，数据库错误保留。
func recoveryResource(tx *gorm.DB, a domain.Actor, resource string) error {
	if e := require(a, "expense.integration.manage"); e != nil {
		return e
	}
	_, all, scopeErr := scopeOrganizations(tx, a)
	if scopeErr != nil {
		return scopeErr
	}
	if all {
		return nil
	}
	var row store.Claim
	e := tx.Where("tenant_id = ? AND id = ?", a.TenantID, resource).First(&row).Error
	if e == nil {
		return requireFinancial(tx, a, domain.Claim{TenantID: row.TenantID, OrgID: row.OrgID}, "expense.integration.manage")
	}
	if !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	var archive store.Archive
	if err := tx.Where("tenant_id = ? AND id = ?", a.TenantID, resource).First(&archive).Error; err == nil {
		c, _, err := loadClaim(tx, a.TenantID, archive.ClaimID, false)
		if err != nil {
			return err
		}
		return requireFinancial(tx, a, c, "expense.integration.manage")
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var job store.Job
	if err := tx.Where("tenant_id = ? AND id = ? AND kind = ?", a.TenantID, resource, "EXPORT").First(&job).Error; err == nil {
		_, all, err := scopeOrganizations(tx, a)
		if err != nil {
			return err
		}
		if all {
			return nil
		}
		var data struct {
			ClaimIDs []string `json:"claim_ids"`
		}
		if err = json.Unmarshal([]byte(job.PayloadJSON), &data); err != nil {
			return err
		}
		if len(data.ClaimIDs) == 0 {
			return domain.Fail("RESOURCE_NOT_FOUND", "此导出没有可核验的完整组织范围", 404)
		}
		for _, id := range data.ClaimIDs {
			if id == resource {
				return domain.Fail("RECOVERY_RESOURCE_INVALID", "恢复资源循环引用", 409)
			}
			c, _, err := loadClaim(tx, a.TenantID, id, false)
			if err != nil {
				return err
			}
			if err = requireFinancial(tx, a, c, "expense.integration.manage"); err != nil {
				return err
			}
		}
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var doc store.Document
	if e = tx.Where("tenant_id = ? AND id = ?", a.TenantID, resource).First(&doc).Error; e != nil {
		return domain.Fail("RESOURCE_NOT_FOUND", "恢复对象不存在或不在范围内", 404)
	}
	rows, e := configs(tx, a.TenantID, "employee_profiles", true)
	if e != nil {
		return e
	}
	for _, r := range rows {
		var p domain.EmployeeProfile
		if e = json.Unmarshal([]byte(r.ContentJSON), &p); e != nil {
			return e
		}
		if p.Active && p.SubjectID == doc.Owner {
			ok, err := financeScope(tx, a, p.OrgID)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
	}
	return domain.Fail("RESOURCE_NOT_FOUND", "恢复对象不存在或不在范围内", 404)
}

// RecoverIntegration 以版本、原因和不可变幂等账本受控重新入队。
// 参数：ctx 为请求边界，a 为当前主体，kind/id 为队列或单据，in 为原因及命令。
// 返回值：冻结恢复回执；不修改载荷、付款效果或财务版本。
func (s *Service) RecoverIntegration(ctx context.Context, a domain.Actor, kind, id string, in RecoveryInput) (json.RawMessage, error) {
	if e := require(a, "expense.integration.manage"); e != nil {
		return nil, e
	}
	if strings.TrimSpace(in.CommandID) == "" || len(in.CommandID) > 128 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 1000 || in.ExpectedVersion < 1 {
		return nil, domain.Fail("RECOVERY_INPUT_INVALID", "恢复需要原因、命令标识和当前版本", 422)
	}
	if kind != "outbox" && kind != "job" && kind != "snapshot" && kind != "rebuild" {
		return nil, domain.Fail("RECOVERY_KIND_INVALID", "不支持此恢复动作", 422)
	}
	hash, e := digest([]any{kind, id, in})
	if e != nil {
		return nil, e
	}
	var result json.RawMessage
	e = localTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var old store.RecoveryCommand
		err := tx.Where("tenant_id = ? AND actor = ? AND command_id = ?", a.TenantID, a.SubjectID, in.CommandID).First(&old).Error
		if err == nil {
			if old.RequestHash != hash {
				return domain.Fail("COMMAND_PAYLOAD_CONFLICT", "命令已用于不同恢复请求", 409)
			}
			if err = recoveryResource(tx, a, old.ResourceID); err != nil {
				return err
			}
			result = json.RawMessage(old.ResultJSON)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		resource, version, err := s.recoverQueue(tx, a, kind, id, in)
		if err != nil {
			return err
		}
		receipt := map[string]any{"id": id, "resource_id": resource, "kind": kind, "status": "PENDING", "version": version, "command_id": in.CommandID}
		raw, err := encode(receipt)
		if err != nil {
			return err
		}
		rid, err := newID()
		if err != nil {
			return err
		}
		if err = tx.Create(&store.RecoveryCommand{ID: rid, TenantID: a.TenantID, Actor: a.SubjectID, CommandID: in.CommandID, ResourceID: resource, Kind: kind, Reason: in.Reason, RequestHash: hash, ResultJSON: raw}).Error; err != nil {
			return err
		}
		if err = appendOutbox(tx, a.TenantID, "audit", resource, map[string]any{"actor_subject_id": a.SubjectID, "action": "integration." + kind + ".retry", "command_id": in.CommandID, "reason": in.Reason, "request_id": a.RequestID, "result": "SUCCESS"}); err != nil {
			return err
		}
		result = json.RawMessage(raw)
		return nil
	})
	if e != nil {
		var committed store.RecoveryCommand
		if lookup := s.db.WithContext(ctx).Where("tenant_id = ? AND actor = ? AND command_id = ?", a.TenantID, a.SubjectID, in.CommandID).First(&committed).Error; lookup == nil && committed.RequestHash == hash {
			if access := recoveryResource(s.db.WithContext(ctx), a, committed.ResourceID); access != nil {
				return nil, access
			}
			return json.RawMessage(committed.ResultJSON), nil
		}
	}
	return result, e
}

// recoverQueue 在范围和租约边界内恢复队列，快照仅使用当前已冻结权威载荷。
// 参数：tx 为事务，a 为主体，kind/id 为对象，in 为版本与理由。
// 返回值：归属对象、新版本和错误。
func (s *Service) recoverQueue(tx *gorm.DB, a domain.Actor, kind, id string, in RecoveryInput) (string, int64, error) {
	if kind == "rebuild" {
		return s.queueRebuild(tx, a, id, in)
	}
	table := "exp_outbox"
	if kind == "job" {
		table = "exp_job"
	}
	var row store.Outbox
	q := tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", a.TenantID)
	if kind == "snapshot" {
		c, _, e := loadClaim(tx, a.TenantID, id, true)
		if e != nil {
			return "", 0, e
		}
		authoritative, e := snapshotOutbox(tx, a.TenantID, id, uint64(c.ProjectionRevision))
		if e != nil {
			return "", 0, e
		}
		q = q.Where("id = ?", authoritative.ID)
	} else {
		q = q.Where("id = ?", id)
	}
	if e := q.Take(&row).Error; e != nil {
		return "", 0, domain.Fail("RESOURCE_NOT_FOUND", "恢复对象不存在", 404)
	}
	if e := recoveryResource(tx, a, row.ResourceID); e != nil {
		return "", 0, e
	}
	if e := verifyVersion(row.Version, in.ExpectedVersion); e != nil {
		return "", 0, e
	}
	if row.Status != "FAILED" && (kind != "snapshot" || row.Status != "DONE") {
		return "", 0, domain.Fail("RECOVERY_STATE_CONFLICT", "仅失败队列或已完成的权威快照可重投", 409)
	}
	if kind == "snapshot" {
		c, _, e := loadClaim(tx, a.TenantID, row.ResourceID, true)
		if e != nil {
			return "", 0, e
		}
		var snap domain.SpendSnapshot
		if e = json.Unmarshal([]byte(row.PayloadJSON), &snap); e != nil {
			return "", 0, e
		}
		if snap.TenantID != a.TenantID || snap.ClaimID != c.ID || snap.ProjectionRevision != uint64(c.ProjectionRevision) || domain.ValidateSpendSnapshot(snap) != nil {
			return "", 0, domain.Fail("AUTHORITATIVE_SNAPSHOT_MISSING", "当前财务版本的完整权威快照缺失，禁止构造替代效果", 409)
		}
	}
	r := tx.Table(table).Where("tenant_id = ? AND id = ? AND version = ? AND status = ?", a.TenantID, row.ID, row.Version, row.Status).Updates(map[string]any{"status": "PENDING", "version": row.Version + 1, "attempt": 0, "last_error": "", "next_attempt_at": time.Now().UTC(), "lease_owner": "", "lease_until": nil, "updated_at": time.Now().UTC()})
	if r.Error != nil {
		return "", 0, r.Error
	}
	if r.RowsAffected != 1 {
		return "", 0, domain.Fail("VERSION_CONFLICT", "队列已被更新", 409)
	}
	return row.ResourceID, row.Version + 1, nil
}
