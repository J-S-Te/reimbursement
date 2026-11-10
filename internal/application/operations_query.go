package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// OperationsQuery 查询授权范围内的死信、同步与对账，不返回冻结业务载荷。
// 参数：ctx 为请求边界，a 为可信主体，kind/id 为查询，params 为有界分页。
// 返回值：状态页或当前单据同步状态及错误。
func (s *Service) OperationsQuery(ctx context.Context, a domain.Actor, kind, id string, params map[string]string) (json.RawMessage, error) {
	db := s.db.WithContext(ctx)
	if kind == "sync-status" {
		c, _, e := loadClaim(db, a.TenantID, id, false)
		if e != nil {
			return nil, e
		}
		if e = s.canRead(db, a, c); e != nil {
			return nil, e
		}
		value, e := s.syncStatus(db, a.TenantID, id)
		if e != nil {
			return nil, e
		}
		return json.Marshal(value)
	}
	claims, e := operationalClaims(db, a)
	if e != nil {
		return nil, e
	}
	page, size, offset := pageArgs(params)
	sub := claims.Select("id")
	var q *gorm.DB
	switch kind {
	case "outboxes":
		q = db.Table("exp_outbox").Where("tenant_id = ?", a.TenantID).Where("status = ?", "FAILED")
		q, e = operationalQueueScope(db, q, a, sub)
		if e != nil {
			return nil, e
		}
	case "jobs":
		q = db.Table("exp_job").Where("tenant_id = ?", a.TenantID).Where("status = ?", "FAILED")
		q, e = operationalQueueScope(db, q, a, sub)
		if e != nil {
			return nil, e
		}
	case "reconciliation":
		q = db.Table("exp_reconciliation_finding").Where("tenant_id = ? AND claim_id IN (?)", a.TenantID, sub)
	default:
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "恢复查询不存在", 404)
	}
	if id != "" {
		q = q.Where("id = ?", id)
	}
	if status := params["status"]; status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if e = q.Count(&total).Error; e != nil {
		return nil, e
	}
	items := []map[string]any{}
	if kind == "reconciliation" {
		var rows []store.ReconciliationFinding
		e = q.Order("updated_at DESC, id DESC").Limit(size).Offset(offset).Find(&rows).Error
		for _, r := range rows {
			items = append(items, map[string]any{"id": r.ID, "claim_id": r.ClaimID, "status": r.Status, "source_revision": r.SourceRevision, "source_hash": r.SourceHash, "remote_revision": r.RemoteRevision, "remote_hash": r.RemoteHash, "remote_consistent": r.RemoteConsistent, "version": r.Version, "updated_at": r.UpdatedAt})
		}
	} else {
		var rows []struct {
			ID, TenantID, ResourceID, Topic, Kind, Status, LastError string
			Version                                                  int64
			Attempt                                                  int
			NextAttemptAt, UpdatedAt                                 time.Time
		}
		e = q.Order("updated_at DESC, id DESC").Limit(size).Offset(offset).Find(&rows).Error
		for _, r := range rows {
			items = append(items, map[string]any{"id": r.ID, "resource_id": r.ResourceID, "topic": r.Topic, "kind": r.Kind, "status": r.Status, "last_error": r.LastError, "version": r.Version, "attempt": r.Attempt, "next_attempt_at": r.NextAttemptAt, "updated_at": r.UpdatedAt})
		}
	}
	if e != nil {
		return nil, e
	}
	return json.Marshal(map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}

// syncStatus 从不可变快照及受理回执推导当前与成功版本。
// 参数：db 为数据库，tenant/id 为单据边界。
// 返回值：财务版本、受理版本和最近投递状态。
func (s *Service) syncStatus(db *gorm.DB, tenant, id string) (any, error) {
	c, _, e := loadClaim(db, tenant, id, false)
	if e != nil {
		return nil, e
	}
	var rows []store.Outbox
	if e = db.Where("tenant_id = ? AND resource_id = ? AND topic IN ?", tenant, id, []string{"project", "project_spend"}).Order("created_at DESC, id DESC").Limit(2001).Find(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) > 2000 {
		return nil, domain.Fail("SYNC_HISTORY_LIMIT", "同步历史超限，请通过对账分页核验", 422)
	}
	var success uint64
	var current *store.Outbox
	for i := range rows {
		r := &rows[i]
		var snap domain.SpendSnapshot
		if e = json.Unmarshal([]byte(r.PayloadJSON), &snap); e != nil {
			return nil, e
		}
		if snap.ProjectionRevision == uint64(c.ProjectionRevision) && current == nil {
			current = r
		}
		if r.Status == "DONE" && snap.ProjectionRevision > success {
			success = snap.ProjectionRevision
		}
	}
	out := map[string]any{"claim_id": id, "current_revision": c.ProjectionRevision, "successful_revision": success, "status": "NOT_REQUIRED"}
	if current != nil {
		out["outbox_id"] = current.ID
		out["status"] = current.Status
		out["version"] = current.Version
		out["last_error"] = current.LastError
		out["next_attempt_at"] = current.NextAttemptAt
		out["attempt"] = current.Attempt
	} else if c.ProjectionRevision > 0 {
		out["status"] = "AUTHORITATIVE_SNAPSHOT_MISSING"
	}
	return out, nil
}

// queueRebuild 仅对相同权威版本和摘要的损坏投影排队受控重建。
// 参数：tx 为事务，a 为主体，id 为差异记录，in 为当前版本与原因。
// 返回值：所属单据、新差异版本和错误。
func (s *Service) queueRebuild(tx *gorm.DB, a domain.Actor, id string, in RecoveryInput) (string, int64, error) {
	var finding store.ReconciliationFinding
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", a.TenantID, id).First(&finding).Error; e != nil {
		return "", 0, domain.Fail("RESOURCE_NOT_FOUND", "差异记录不存在", 404)
	}
	if e := recoveryResource(tx, a, finding.ClaimID); e != nil {
		return "", 0, e
	}
	if e := verifyVersion(finding.Version, in.ExpectedVersion); e != nil {
		return "", 0, e
	}
	c, _, e := loadClaim(tx, a.TenantID, finding.ClaimID, true)
	if e != nil {
		return "", 0, e
	}
	if uint64(c.ProjectionRevision) != finding.SourceRevision {
		return "", 0, domain.Fail("VERSION_CONFLICT", "财务版本已改变，请重新对账", 409)
	}
	snapshot, e := snapshotOutbox(tx, a.TenantID, c.ID, finding.SourceRevision)
	if e != nil {
		return "", 0, e
	}
	var frozen domain.SpendSnapshot
	if e = json.Unmarshal([]byte(snapshot.PayloadJSON), &frozen); e != nil {
		return "", 0, e
	}
	if frozen.SnapshotHash != finding.SourceHash {
		return "", 0, domain.Fail("HISTORY_CONFLICT", "对账来源摘要不再一致，禁止覆盖历史", 409)
	}
	switch finding.Status {
	case "REMOTE_MISSING", "REMOTE_BEHIND":
		retry := in
		retry.ExpectedVersion = snapshot.Version
		if _, _, e = s.recoverQueue(tx, a, "snapshot", c.ID, retry); e != nil {
			return "", 0, e
		}
	case "PROJECTION_CORRUPT":
		if finding.SourceRevision != finding.RemoteRevision || finding.SourceHash != finding.RemoteHash {
			return "", 0, domain.Fail("RECONCILIATION_CONFLICT", "同版本摘要不一致，禁止覆盖冲突历史", 409)
		}
		if e = appendOutbox(tx, a.TenantID, "project_rebuild", c.ID, map[string]any{"claim_id": c.ID, "reason": in.Reason}); e != nil {
			return "", 0, e
		}
	default:
		return "", 0, domain.Fail("RECONCILIATION_CONFLICT", "此差异需先核验权威历史，不能自动修复", 409)
	}
	r := tx.Model(&store.ReconciliationFinding{}).Where("tenant_id = ? AND id = ? AND version = ?", a.TenantID, id, finding.Version).Updates(map[string]any{"status": "REPAIR_QUEUED", "version": finding.Version + 1})
	if r.Error != nil {
		return "", 0, r.Error
	}
	if r.RowsAffected != 1 {
		return "", 0, domain.Fail("VERSION_CONFLICT", "差异记录已改变", 409)
	}
	return c.ID, finding.Version + 1, nil
}

// QueryRecoveryCommand 查询当前主体的冻结恢复回执并复核当前操作范围。
// 参数：ctx 为查询边界，a 为可信主体，commandID 为原命令标识。
// 返回值：不可变回执；无记录或已撤权时拒绝。
func (s *Service) QueryRecoveryCommand(ctx context.Context, a domain.Actor, commandID string) (json.RawMessage, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	db := s.db.WithContext(ctx)
	var row store.RecoveryCommand
	e := db.Where("tenant_id = ? AND actor = ? AND command_id = ?", a.TenantID, a.SubjectID, commandID).First(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return nil, domain.Fail("COMMAND_NOT_FOUND", "此恢复命令尚无已提交结果，请使用原命令重试", 404)
	}
	if e != nil {
		return nil, e
	}
	if e = require(a, "expense.integration.manage"); e != nil {
		return nil, e
	}
	if e = recoveryResource(db, a, row.ResourceID); e != nil {
		return nil, e
	}
	return json.RawMessage(row.ResultJSON), nil
}

// operationalQueueScope 将解析和档案资源映射到当前组织范围后再分页。
// 参数：db 为数据库，q 为队列查询，a 为主体，claims 为有范围的单据子查询。
// 返回值：过滤查询及配置错误，完整租户范围仍不会跨租户。
func operationalQueueScope(db, q *gorm.DB, a domain.Actor, claims *gorm.DB) (*gorm.DB, error) {
	orgs, all, e := scopeOrganizations(db, a)
	if e != nil {
		return nil, e
	}
	if all {
		return q, nil
	}
	allowed := map[string]bool{}
	for _, org := range orgs {
		allowed[org] = true
	}
	profiles, e := configs(db, a.TenantID, "employee_profiles", true)
	if e != nil {
		return nil, e
	}
	owners := []string{}
	for _, r := range profiles {
		var p domain.EmployeeProfile
		if e = json.Unmarshal([]byte(r.ContentJSON), &p); e != nil {
			return nil, e
		}
		if p.Active && allowed[p.OrgID] {
			owners = append(owners, p.SubjectID)
		}
	}
	archives := db.Model(&store.Archive{}).Select("id").Where("tenant_id = ? AND claim_id IN (?)", a.TenantID, claims)
	condition := db.Where("resource_id IN (?) OR resource_id IN (?)", claims, archives)
	if len(owners) > 0 {
		docs := db.Model(&store.Document{}).Select("id").Where("tenant_id = ? AND owner IN ?", a.TenantID, owners)
		condition = condition.Or("resource_id IN (?)", docs)
	}
	return q.Where(condition), nil
}
