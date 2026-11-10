package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type archiveBorrowRequest struct {
	CommandInput
	Reason        string `json:"reason"`
	RequestedDays int    `json:"requested_days"`
}

type archiveBorrowDecision struct {
	CommandInput
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// RequestArchiveBorrow 申请已有整单范围内的封存档案受控借阅。
// 参数：ctx为请求上下文，a为可信主体，archiveID为不可变档案，raw为申请命令。
// 返回值：申请回执或公开业务错误。
func (s *Service) RequestArchiveBorrow(ctx context.Context, a domain.Actor, archiveID string, raw json.RawMessage) (any, error) {
	var input archiveBorrowRequest
	if e := json.Unmarshal(raw, &input); e != nil {
		return nil, domain.Fail("INVALID_REQUEST", "借阅申请格式不正确", 422)
	}
	if input.RequestedDays == 0 {
		input.RequestedDays = 7
	}
	if input.RequestedDays < 1 || input.RequestedDays > 7 || strings.TrimSpace(input.Reason) == "" || utf8.RuneCountInString(input.Reason) > 2000 {
		return nil, domain.Fail("INVALID_ARCHIVE_BORROW", "请填写借阅原因，期限须为1至7天", 422)
	}
	cc, e := borrowCommand(a, "archive.borrow.request", archiveID, input.CommandInput, raw)
	if e != nil {
		return nil, e
	}
	return s.runBorrowCommand(ctx, cc, func(tx *gorm.DB) (store.ArchiveBorrow, error) {
		archive, e := loadBorrowArchive(tx, a.TenantID, archiveID)
		if e != nil {
			return store.ArchiveBorrow{}, e
		}
		if e = s.requireBorrowRead(tx, a, archive.ClaimID); e != nil {
			return store.ArchiveBorrow{}, e
		}
		if e = verifyVersion(archive.PackageVersion, input.ExpectedVersion); e != nil {
			return store.ArchiveBorrow{}, e
		}
		id, e := newID()
		if e != nil {
			return store.ArchiveBorrow{}, e
		}
		row := store.ArchiveBorrow{ID: id, TenantID: a.TenantID, ArchiveID: archive.ID,
			ClaimID: archive.ClaimID, ArchiveVersion: archive.PackageVersion, ManifestHash: archive.ManifestHash,
			Requester: a.SubjectID, RequesterTenantAll: a.Has("expense.scope.tenant_all"),
			Reason: strings.TrimSpace(input.Reason), RequestedDays: input.RequestedDays, Status: "PENDING", Version: 1}
		e = tx.Create(&row).Error
		return row, e
	})
}

// DecideArchiveBorrow 独立审批借阅并原子排入真实审批人建包任务。
// 参数：ctx为请求上下文，a为当前审批人，id为借阅，raw为批准或拒绝命令。
// 返回值：冻结审批回执或业务错误。
func (s *Service) DecideArchiveBorrow(ctx context.Context, a domain.Actor, id string, raw json.RawMessage) (any, error) {
	var input archiveBorrowDecision
	if e := json.Unmarshal(raw, &input); e != nil {
		return nil, domain.Fail("INVALID_REQUEST", "借阅审批格式不正确", 422)
	}
	if input.Decision != "APPROVE" && input.Decision != "REJECT" || strings.TrimSpace(input.Reason) == "" || utf8.RuneCountInString(input.Reason) > 2000 {
		return nil, domain.Fail("INVALID_ARCHIVE_BORROW_DECISION", "请选择批准或拒绝并填写原因", 422)
	}
	cc, e := borrowCommand(a, "archive.borrow.decide", id, input.CommandInput, raw)
	if e != nil {
		return nil, e
	}
	db := s.db.WithContext(ctx)
	if value, found, e := s.replayBorrowCommand(db, cc); found || e != nil {
		return value, e
	}
	row, e := loadArchiveBorrow(db, a.TenantID, id, false)
	if e != nil {
		return nil, e
	}
	if e = s.requireBorrowReviewer(db, a, row); e != nil {
		return nil, e
	}
	// 批准授予借阅资格必须实时核验申请人；拒绝只关闭申请，不能被其撤权阻断。
	var requester domain.Actor
	if input.Decision == "APPROVE" {
		requester, e = s.currentBorrowRequester(ctx, a, row)
		if e != nil {
			return nil, e
		}
	}
	return s.runBorrowCommand(ctx, cc, func(tx *gorm.DB) (store.ArchiveBorrow, error) {
		return s.decideArchiveBorrow(tx, a, requester, id, input)
	})
}

// borrowCommand 校验命令边界并冻结包含目标和完整载荷的规范摘要。
// 参数：a为可信身份，operation/target为固定动作与目标，input为命令头，raw为完整载荷。
// 返回值：稳定命令上下文或格式错误。
func borrowCommand(a domain.Actor, operation, target string, input CommandInput, raw json.RawMessage) (commandContext, error) {
	cc := commandContext{actor: a, operation: operation, resourceID: target, input: input}
	if e := require(a, "expense.archive.read"); e != nil {
		return cc, e
	}
	if e := require(a, "expense.claim.read"); e != nil {
		return cc, e
	}
	if strings.TrimSpace(input.CommandID) == "" || len(input.CommandID) > 128 || target == "" || len(target) > 64 {
		return cc, domain.Fail("COMMAND_ID_REQUIRED", "请提供有效命令和档案标识", 422)
	}
	var payload any
	if e := json.Unmarshal(raw, &payload); e != nil {
		return cc, domain.Fail("INVALID_REQUEST", "请求格式不正确", 422)
	}
	hash, e := digest([]any{operation, target, payload})
	cc.hash = hash
	return cc, e
}

// runBorrowCommand 将借阅状态、真实任务、冻结回执和可靠审计原子提交。
// 参数：ctx为请求边界，cc为规范命令，work为只含本地数据库操作的业务函数。
// 返回值：唯一冻结回执；重复竞争从已有成功命令恢复，失败不制造成功回执。
func (s *Service) runBorrowCommand(ctx context.Context, cc commandContext, work func(*gorm.DB) (store.ArchiveBorrow, error)) (any, error) {
	db := s.db.WithContext(ctx)
	if value, found, e := s.replayBorrowCommand(db, cc); found || e != nil {
		return value, e
	}
	var result any
	e := localTransaction(ctx, db, func(tx *gorm.DB) error {
		if value, found, e := s.replayBorrowCommand(tx, cc); found || e != nil {
			result = value
			return e
		}
		row, e := work(tx)
		if e != nil {
			return e
		}
		raw, e := encode(row)
		if e != nil {
			return e
		}
		id, e := newID()
		if e != nil {
			return e
		}
		command := store.Command{ID: id, TenantID: cc.actor.TenantID, Actor: cc.actor.SubjectID,
			Operation: cc.operation, CommandID: cc.input.CommandID, RequestHash: cc.hash,
			ResourceID: row.ID, ResultJSON: raw, CreatedAt: time.Now().UTC()}
		if e = tx.Create(&command).Error; e != nil {
			return e
		}
		if e = appendBorrowAudit(tx, cc.actor, row, cc.operation, cc.input.CommandID, ""); e != nil {
			return e
		}
		result = row
		return nil
	})
	if e != nil {
		if value, found, replayErr := s.replayBorrowCommand(db, cc); found {
			return value, replayErr
		}
	}
	if e != nil {
		return nil, e
	}
	return result, nil
}

// replayBorrowCommand 重放前重新校验当前档案范围，并拒绝同标识异载荷。
// 参数：db为当前事务或数据库，cc为当前身份和请求摘要。
// 返回值：原始冻结回执、命中标记及可见性或冲突错误。
func (s *Service) replayBorrowCommand(db *gorm.DB, cc commandContext) (any, bool, error) {
	var command store.Command
	e := db.Where("tenant_id = ? AND actor = ? AND operation = ? AND command_id = ?",
		cc.actor.TenantID, cc.actor.SubjectID, cc.operation, cc.input.CommandID).Take(&command).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, e
	}
	if command.RequestHash != cc.hash {
		return nil, true, domain.Fail("COMMAND_PAYLOAD_CONFLICT", "命令标识已用于不同借阅请求", 409)
	}
	row, e := loadArchiveBorrow(db, cc.actor.TenantID, command.ResourceID, false)
	if e != nil {
		return nil, true, e
	}
	if cc.operation == "archive.borrow.decide" {
		e = s.requireBorrowReviewer(db, cc.actor, row)
	} else {
		e = s.authorizeBorrow(db, cc.actor, row)
	}
	if e != nil {
		return nil, true, e
	}
	return json.RawMessage(command.ResultJSON), true, nil
}

// loadArchiveBorrow 以租户和借阅标识加载事实，可对审批状态加行锁。
// 参数：db为当前数据库，tenant/id为可信边界，lock表示写入事务。
// 返回值：持久借阅或隐藏对象错误。
func loadArchiveBorrow(db *gorm.DB, tenant, id string, lock bool) (store.ArchiveBorrow, error) {
	var row store.ArchiveBorrow
	q := db.Where("tenant_id = ? AND id = ?", tenant, id)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	e := q.Take(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		e = domain.Fail("RESOURCE_NOT_FOUND", "借阅申请不存在或无权访问", 404)
	}
	return row, e
}

// loadBorrowArchive 核对真实封存清单内容摘要，不把未封存或漂移版本用于借阅。
// 参数：db为当前数据库，tenant/id为可信租户和档案版本。
// 返回值：封存档案或归属、状态、摘要冲突错误。
func loadBorrowArchive(db *gorm.DB, tenant, id string) (store.Archive, error) {
	var row store.Archive
	e := db.Where("tenant_id = ? AND id = ?", tenant, id).Take(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return row, domain.Fail("RESOURCE_NOT_FOUND", "档案不存在或无权访问", 404)
	}
	if e != nil {
		return row, e
	}
	sum := sha256.Sum256([]byte(row.ManifestJSON))
	if row.Status != "SEALED" || row.ManifestJSON == "" || row.ManifestHash != hex.EncodeToString(sum[:]) {
		return row, domain.Fail("ARCHIVE_BORROW_BINDING_CONFLICT", "档案未完整封存或清单摘要不一致", 409)
	}
	return row, nil
}

// requireBorrowRead 校验当前读取能力与整单范围，不要求申请人拥有导出权限。
// 参数：db为数据库，a为当前真实身份，claim为档案归属单据。
// 返回值：读取能力或对象范围不满足时拒绝。
func (s *Service) requireBorrowRead(db *gorm.DB, a domain.Actor, claim string) error {
	for _, permission := range []string{"expense.archive.read", "expense.claim.read"} {
		if e := require(a, permission); e != nil {
			return e
		}
	}
	c, _, e := loadClaim(db, a.TenantID, claim, false)
	if e != nil {
		return e
	}
	full, e := s.fullRead(db, a, c)
	if e != nil {
		return e
	}
	if !full {
		return domain.Fail("FORBIDDEN", "当前没有完整档案读取范围", 403)
	}
	return nil
}

// checkBorrowBinding 复核借阅冻结的租户、单据、档案版本和清单摘要。
// 参数：db为数据库，row为当前租户已加载借阅。
// 返回值：真实封存档案，任何绑定漂移均失败关闭。
func checkBorrowBinding(db *gorm.DB, row store.ArchiveBorrow) (store.Archive, error) {
	archive, e := loadBorrowArchive(db, row.TenantID, row.ArchiveID)
	if e != nil {
		return archive, e
	}
	if archive.ClaimID != row.ClaimID || archive.PackageVersion != row.ArchiveVersion || archive.ManifestHash != row.ManifestHash {
		return archive, domain.Fail("ARCHIVE_BORROW_BINDING_CONFLICT", "借阅所绑定档案版本已不一致", 409)
	}
	return archive, nil
}

// requireBorrowReviewer 约束独立审批人的当前真实导出能力和同一档案整单范围。
// 参数：db为数据库，a为当前审批身份，row为借阅。
// 返回值：自审、撤权或范围不满足时拒绝。
func (s *Service) requireBorrowReviewer(db *gorm.DB, a domain.Actor, row store.ArchiveBorrow) error {
	if a.TenantID != row.TenantID {
		return domain.Fail("RESOURCE_NOT_FOUND", "借阅申请不存在", 404)
	}
	if a.SubjectID == row.Requester {
		return domain.Fail("ARCHIVE_BORROW_SELF_REVIEW", "借阅申请须由独立人员审批", 403)
	}
	if e := s.requireArchivePackage(db, a, row.ClaimID); e != nil {
		return e
	}
	_, e := checkBorrowBinding(db, row)
	return e
}

// authorizeBorrow 限制详情与列表仅当前申请人或有同档案完整范围的独立审批人可见。
// 参数：db为数据库，a为当前身份，row为借阅事实。
// 返回值：当前范围或清单绑定错误。
func (s *Service) authorizeBorrow(db *gorm.DB, a domain.Actor, row store.ArchiveBorrow) error {
	if a.TenantID != row.TenantID {
		return domain.Fail("RESOURCE_NOT_FOUND", "借阅申请不存在", 404)
	}
	if a.SubjectID != row.Requester {
		return s.requireBorrowReviewer(db, a, row)
	}
	if e := s.requireBorrowRead(db, a, row.ClaimID); e != nil {
		return e
	}
	_, e := checkBorrowBinding(db, row)
	return e
}

// currentBorrowRequester 通过人员目录逐项取得申请人的当前最小能力，不信任申请快照。
// 参数：ctx为目录超时边界，reviewer为可信操作者，row为原申请事实。
// 返回值：经实时核验的只读身份；目录故障或当前撤权拒绝审批。
func (s *Service) currentBorrowRequester(ctx context.Context, reviewer domain.Actor, row store.ArchiveBorrow) (domain.Actor, error) {
	a := domain.Actor{TenantID: row.TenantID, SubjectID: row.Requester}
	if s.deps.Personnel == nil {
		return a, domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员目录不可用，不能批准借阅", 503)
	}
	for _, permission := range []string{"expense.archive.read", "expense.claim.read"} {
		if e := s.deps.Personnel.VerifyApprover(ctx, reviewer, row.Requester, permission); e != nil {
			return a, e
		}
		a.Permissions = append(a.Permissions, permission)
	}
	if row.RequesterTenantAll {
		e := s.deps.Personnel.VerifyApprover(ctx, reviewer, row.Requester, "expense.scope.tenant_all")
		if e == nil {
			a.Permissions = append(a.Permissions, "expense.scope.tenant_all")
		} else {
			var p *domain.Problem
			if !errors.As(e, &p) || p.Status != 422 {
				return a, e
			}
		}
	}
	return a, nil
}

// decideArchiveBorrow 在持有行锁时以CAS批准或拒绝，真实任务与借阅状态同事务。
// 参数：tx为本地事务，a为当前审批人，requester为实时最小身份，id为借阅，input为审批。
// 返回值：新事实；建包、范围或状态失败全部回滚。
func (s *Service) decideArchiveBorrow(tx *gorm.DB, a, requester domain.Actor, id string, input archiveBorrowDecision) (store.ArchiveBorrow, error) {
	row, e := loadArchiveBorrow(tx, a.TenantID, id, true)
	if e != nil {
		return row, e
	}
	if e = s.requireBorrowReviewer(tx, a, row); e != nil {
		return row, e
	}
	if input.Decision == "APPROVE" {
		if row.Requester != requester.SubjectID || requester.TenantID != row.TenantID {
			return row, domain.Fail("FORBIDDEN", "借阅申请人绑定不一致", 403)
		}
		if e = s.requireBorrowRead(tx, requester, row.ClaimID); e != nil {
			return row, e
		}
	}
	if e = verifyVersion(row.Version, input.ExpectedVersion); e != nil {
		return row, e
	}
	if row.Status != "PENDING" {
		return row, domain.Fail("STATE_CONFLICT", "借阅申请已经处理", 409)
	}
	previous := row.Version
	row.Version++
	row.Approver, row.DecisionReason = a.SubjectID, strings.TrimSpace(input.Reason)
	row.Status = "REJECTED"
	if input.Decision == "APPROVE" {
		cc := &commandContext{actor: a, resourceID: row.ArchiveID, input: CommandInput{ExpectedVersion: row.ArchiveVersion}}
		queued, _, e := s.queueArchivePackage(tx, cc)
		if e != nil {
			return row, e
		}
		row.JobID = queued.(map[string]any)["job_id"].(string)
		now := time.Now().UTC()
		expires := now.Add(time.Duration(row.RequestedDays) * 24 * time.Hour)
		row.Status, row.ApprovedAt, row.ExpiresAt = "APPROVED", &now, &expires
	}
	row.UpdatedAt = time.Now().UTC()
	update := tx.Model(&store.ArchiveBorrow{}).
		Where("tenant_id = ? AND id = ? AND version = ? AND status = ?",
			a.TenantID, id, previous, "PENDING").Updates(map[string]any{
		"status": row.Status, "version": row.Version, "approver": row.Approver,
		"decision_reason": row.DecisionReason, "job_id": row.JobID, "approved_at": row.ApprovedAt,
		"expires_at": row.ExpiresAt, "updated_at": row.UpdatedAt,
	})
	if update.Error != nil {
		return row, update.Error
	}
	if update.RowsAffected != 1 {
		return row, domain.Fail("VERSION_CONFLICT", "借阅状态已改变，请刷新", 409)
	}
	return row, nil
}

// appendBorrowAudit 与业务事务可靠记录借阅事实或每次实际下载。
// 参数：tx为数据库事务，a为真实操作者，row为借阅，action为动作，command为稳定命令，packageSHA为下载内容摘要。
// 返回值：审计不可落库时返回错误，不吞错或返回成功文件。
func appendBorrowAudit(tx *gorm.DB, a domain.Actor, row store.ArchiveBorrow, action, command, packageSHA string) error {
	return appendOutbox(tx, a.TenantID, "audit", row.ClaimID, map[string]any{
		"actor_subject_id": a.SubjectID, "request_id": a.RequestID, "action": action, "command_id": command,
		"borrow_id": row.ID, "archive_id": row.ArchiveID, "manifest_hash": row.ManifestHash,
		"job_id": row.JobID, "result": "SUCCESS", "package_sha256": packageSHA,
	})
}

// ArchiveBorrows 列出当前真实范围内的本人申请或可审批申请。
// 参数：ctx为请求上下文，a为可信身份，params为有限状态、档案、单据筛选和分页。
// 返回值：借阅分页或权限错误，列表不读取包字节。
func (s *Service) ArchiveBorrows(ctx context.Context, a domain.Actor, params map[string]string) (any, error) {
	if e := require(a, "expense.archive.read"); e != nil {
		return nil, e
	}
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	db := s.db.WithContext(ctx)
	claims, e := readableClaims(db, a, false)
	if e != nil {
		return nil, e
	}
	q := db.Model(&store.ArchiveBorrow{}).Where("tenant_id = ? AND claim_id IN (?)", a.TenantID, claims.Select("id"))
	if !a.Has("expense.archive.export") {
		q = q.Where("requester = ?", a.SubjectID)
	}
	for key, column := range map[string]string{"archive_id": "archive_id", "claim_id": "claim_id"} {
		if value := params[key]; value != "" {
			if len(value) > 64 {
				return nil, domain.Fail("INVALID_QUERY_FILTER", "查询标识过长", 422)
			}
			q = q.Where(column+" = ?", value)
		}
	}
	switch params["status"] {
	case "":
	case "PENDING", "REJECTED":
		q = q.Where("status = ?", params["status"])
	case "APPROVED":
		q = q.Where("status = ? AND expires_at > ?", "APPROVED", time.Now().UTC())
	case "EXPIRED":
		q = q.Where("status = ? AND expires_at <= ?", "APPROVED", time.Now().UTC())
	default:
		return nil, domain.Fail("INVALID_QUERY_FILTER", "借阅状态不合法", 422)
	}
	var total int64
	if e = q.Count(&total).Error; e != nil {
		return nil, e
	}
	page, size, offset := pageArgs(params)
	var rows []store.ArchiveBorrow
	if e = q.Order("created_at DESC, id DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		if e = s.authorizeBorrow(db, a, row); e != nil {
			return nil, e
		}
		view, e := borrowView(db, a, row)
		if e != nil {
			return nil, e
		}
		items = append(items, view)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// ArchiveBorrow 读取单笔当前可见借阅与实际包状态。
// 参数：ctx为请求上下文，a为可信身份，id为借阅。
// 返回值：借阅详情或权限错误，不返回生成物字节。
func (s *Service) ArchiveBorrow(ctx context.Context, a domain.Actor, id string) (any, error) {
	if e := require(a, "expense.archive.read"); e != nil {
		return nil, e
	}
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	db := s.db.WithContext(ctx)
	row, e := loadArchiveBorrow(db, a.TenantID, id, false)
	if e != nil {
		return nil, e
	}
	if e = s.authorizeBorrow(db, a, row); e != nil {
		return nil, e
	}
	return borrowView(db, a, row)
}

// borrowView 解释期限与真实建包状态并输出当前可用动作。
// 参数：db为数据库，a为已授权当前身份，row为借阅事实。
// 返回值：详情字段；生成物已清理明确EXPIRED，不伪造可下载状态。
func borrowView(db *gorm.DB, a domain.Actor, row store.ArchiveBorrow) (any, error) {
	raw, e := json.Marshal(row)
	if e != nil {
		return nil, e
	}
	var view map[string]any
	if e = json.Unmarshal(raw, &view); e != nil {
		return nil, e
	}
	actions := []string{}
	active := row.Status == "APPROVED" && row.ExpiresAt != nil && time.Now().Before(*row.ExpiresAt)
	if row.Status == "APPROVED" && !active {
		view["status"] = "EXPIRED"
	}
	if row.Status == "PENDING" && a.SubjectID != row.Requester && a.Has("expense.archive.export") {
		actions = []string{"APPROVE", "REJECT"}
	}
	view["package_status"] = "NOT_REQUESTED"
	if row.JobID != "" {
		job, e := loadBorrowPackageJob(db, row)
		if e != nil {
			return nil, e
		}
		view["package_status"], view["last_error"] = job.Status, job.LastError
		if job.Status == "DONE" {
			var count int64
			e = db.Model(&store.ArchiveDownload{}).Where("tenant_id = ? AND job_id = ? AND archive_id = ? AND claim_id = ? AND manifest_hash = ?", row.TenantID, row.JobID, row.ArchiveID, row.ClaimID, row.ManifestHash).Count(&count).Error
			if e != nil {
				return nil, e
			}
			if count == 0 {
				view["package_status"] = "EXPIRED"
			} else if active && a.SubjectID == row.Requester {
				actions = append(actions, "DOWNLOAD")
			}
		}
	}
	view["allowed_actions"] = actions
	return view, nil
}

// loadBorrowPackageJob 校验批准时真实审批人建包任务与唯一档案绑定。
// 参数：db为当前数据库，row为已批准借阅。
// 返回值：绑定任务；错主体、档案或摘要均拒绝。
func loadBorrowPackageJob(db *gorm.DB, row store.ArchiveBorrow) (store.Job, error) {
	var job store.Job
	e := db.Where("tenant_id = ? AND id = ? AND kind = ? AND resource_id = ?", row.TenantID, row.JobID, "ARCHIVE_PACKAGE", row.ClaimID).Take(&job).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return job, domain.Fail("ARCHIVE_BORROW_PACKAGE_MISMATCH", "借阅建包任务不存在或绑定不一致", 409)
	}
	if e != nil {
		return job, e
	}
	var payload struct {
		Actor        domain.Actor `json:"actor"`
		ArchiveID    string       `json:"archive_id"`
		ManifestHash string       `json:"manifest_hash"`
	}
	if e = json.Unmarshal([]byte(job.PayloadJSON), &payload); e != nil {
		return job, e
	}
	if payload.Actor.TenantID != row.TenantID || payload.Actor.SubjectID != row.Approver || !payload.Actor.Has("expense.archive.export") || payload.ArchiveID != row.ArchiveID || payload.ManifestHash != row.ManifestHash {
		return job, domain.Fail("ARCHIVE_BORROW_PACKAGE_MISMATCH", "借阅包与批准档案绑定不一致", 409)
	}
	return job, nil
}

// ReadArchiveBorrow 逐次复核当前权限、有效批准、包绑定和摘要后审计下载。
// 参数：ctx为请求上下文，a为申请人当前身份，id为借阅。
// 返回值：固定文件名和真实ZIP字节，任何核验或审计失败均不返回文件。
func (s *Service) ReadArchiveBorrow(ctx context.Context, a domain.Actor, id string) (string, []byte, error) {
	if e := require(a, "expense.archive.read"); e != nil {
		return "", nil, e
	}
	if e := require(a, "expense.claim.read"); e != nil {
		return "", nil, e
	}
	var content []byte
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, e := loadArchiveBorrow(tx, a.TenantID, id, false)
		if e != nil {
			return e
		}
		if a.SubjectID != row.Requester {
			return domain.Fail("FORBIDDEN", "借阅包仅限申请人下载", 403)
		}
		if e = s.authorizeBorrow(tx, a, row); e != nil {
			return e
		}
		if row.Status != "APPROVED" || row.ExpiresAt == nil || !time.Now().Before(*row.ExpiresAt) {
			return domain.Fail("ARCHIVE_BORROW_EXPIRED", "借阅未批准或已到期，请重新申请", 409)
		}
		job, e := loadBorrowPackageJob(tx, row)
		if e != nil {
			return e
		}
		if job.Status != "DONE" {
			return domain.Fail("ARCHIVE_PACKAGE_NOT_READY", "借阅档案包尚未完成", 409)
		}
		var result store.ArchiveDownload
		e = tx.Where("tenant_id = ? AND job_id = ?", a.TenantID, row.JobID).Take(&result).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return domain.Fail("ARCHIVE_PACKAGE_EXPIRED", "借阅包已超过保留期，请重新申请", 409)
		}
		if e != nil {
			return e
		}
		sum := sha256.Sum256(result.Content)
		if result.ArchiveID != row.ArchiveID || result.ClaimID != row.ClaimID || result.ManifestHash != row.ManifestHash || hex.EncodeToString(sum[:]) != result.SHA256 {
			return domain.Fail("ARCHIVE_PACKAGE_HASH_MISMATCH", "借阅档案包绑定或内容校验失败", 409)
		}
		if e = appendBorrowAudit(tx, a, row, "archive.borrow.download", row.ID, result.SHA256); e != nil {
			return e
		}
		content = result.Content
		return nil
	})
	if e != nil {
		return "", nil, e
	}
	return "reimbursement-borrow-" + id + ".zip", content, nil
}
