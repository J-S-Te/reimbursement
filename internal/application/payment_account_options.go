package application

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

var controlledAccountReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/\-]{0,127}$`)
var numericAccountReference = regexp.MustCompile(`^[0-9]+$`)

// validatePaymentAccountConfig 限制目录字段为业务名称和外部受控引用，拒绝账号原文。
// 参数：raw 为已去除命令参数的配置内容。
// 返回值：配置缺失、越界或含原始账号字段时返回公开校验错误。
func validatePaymentAccountConfig(raw string) error {
	invalid := domain.Fail("INVALID_CONFIGURATION", "请填写付款账户名称、付款主体及外部受控引用，勿录入银行账号原文", 422)
	var account domain.PaymentAccount
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &account) != nil || json.Unmarshal([]byte(raw), &fields) != nil {
		return invalid
	}
	allowed := map[string]bool{"id": true, "name": true, "finance_entity_id": true,
		"account_ref": true, "active": true, "version": true, "published": true}
	for key := range fields {
		if !allowed[key] {
			return invalid
		}
	}
	if strings.TrimSpace(account.Name) != account.Name || account.Name == "" ||
		utf8.RuneCountInString(account.Name) > 100 || account.FinanceEntityID == "" ||
		len(account.FinanceEntityID) > 64 || strings.TrimSpace(account.FinanceEntityID) != account.FinanceEntityID ||
		!controlledAccountReference.MatchString(account.AccountRef) || numericAccountReference.MatchString(account.AccountRef) {
		return invalid
	}
	return nil
}

// paymentFinanceEntity 读取当前发布启用且包含该费用组织的付款主体。
// 参数：db 为数据库；c 为单据的可信租户、付款主体和组织边界。
// 返回值：符合范围的付款主体，失效或不适用时返回校验错误。
func paymentFinanceEntity(db *gorm.DB, c domain.Claim) (domain.FinanceEntity, error) {
	rows, err := configs(db, c.TenantID, "finance_entities", true)
	if err != nil {
		return domain.FinanceEntity{}, err
	}
	for _, row := range rows {
		var entity domain.FinanceEntity
		if err = json.Unmarshal([]byte(row.ContentJSON), &entity); err != nil {
			return entity, err
		}
		if entity.ID != c.FinanceEntityID || !entity.Active {
			continue
		}
		for _, org := range entity.OrgIDs {
			if c.OrgID == "" || org == c.OrgID {
				return entity, nil
			}
		}
	}
	return domain.FinanceEntity{}, domain.Fail("PAYMENT_ENTITY_UNAVAILABLE", "付款主体未发布、已停用或不适用当前组织", 422)
}

// validatePaymentAccountEntity 在配置保存或发布时核对付款账户的真实主体归属。
// 参数：db 为数据库；tenant 为可信租户；raw 为账户配置。
// 返回值：付款主体不存在或未启用时拒绝保存，其余错误原样返回。
func validatePaymentAccountEntity(db *gorm.DB, tenant, raw string) error {
	var account domain.PaymentAccount
	if err := json.Unmarshal([]byte(raw), &account); err != nil {
		return err
	}
	_, err := paymentFinanceEntity(db, domain.Claim{TenantID: tenant, FinanceEntityID: account.FinanceEntityID})
	return err
}

// paymentAccountChoices 只返回当前付款主体名下已发布启用且未退休的受控账户。
// 参数：db 为数据库；c 为已授权整单。
// 返回值：按名称排序的真实账户集合，归属或配置异常时拒绝。
func paymentAccountChoices(db *gorm.DB, c domain.Claim) ([]domain.PaymentAccount, error) {
	if _, err := paymentFinanceEntity(db, c); err != nil {
		return nil, err
	}
	rows, err := configs(db, c.TenantID, "payment_accounts", true)
	if err != nil {
		return nil, err
	}
	items := []domain.PaymentAccount{}
	refs := map[string]bool{}
	for _, row := range rows {
		var account domain.PaymentAccount
		if err = json.Unmarshal([]byte(row.ContentJSON), &account); err != nil {
			return nil, err
		}
		if !account.Active || account.FinanceEntityID != c.FinanceEntityID {
			continue
		}
		if err = validatePaymentAccountConfig(row.ContentJSON); err != nil {
			return nil, err
		}
		if refs[account.AccountRef] {
			return nil, domain.Fail("PAYMENT_ACCOUNT_CONFLICT", "付款账户存在重复有效配置，请管理员整理", 422)
		}
		refs[account.AccountRef] = true
		items = append(items, account)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// validatePaymentSource 在正式付款事务内重验账户的当前启用状态和付款主体归属。
// 参数：db 为付款事务；c 为锁定单据；reference 为选中的外部受控账户引用。
// 返回值：未知、停用、退休或其他主体账户均拒绝，不创建付款事实。
func validatePaymentSource(db *gorm.DB, c domain.Claim, reference string) error {
	accounts, err := paymentAccountChoices(db, c)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.AccountRef == reference {
			return nil
		}
	}
	return domain.Fail("PAYMENT_ACCOUNT_UNAVAILABLE", "请选择当前付款主体已启用的受控付款账户", 422)
}

// PaymentOptions 返回当前财务动作可用的真实付款账户和申请人已审核收款档案。
// 参数：ctx/a 为可信调用；claimID 为当前单据坐标。
// 返回值：安全业务选项；缺少整单权限、财务范围或可用动作时拒绝。
func (s *Service) PaymentOptions(ctx context.Context, a domain.Actor, claimID string) (json.RawMessage, error) {
	db := s.db.WithContext(ctx)
	c, err := s.paymentOptionsClaim(db, a, claimID)
	if err != nil {
		return nil, err
	}
	accounts, err := paymentAccountChoices(db, c)
	if err != nil {
		return nil, err
	}
	sources := []map[string]any{}
	for _, account := range accounts {
		sources = append(sources, map[string]any{"id": account.ID, "name": account.Name, "account_ref": account.AccountRef})
	}
	payees, err := paymentPayeeChoices(db, c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"source_accounts": sources, "payee_profiles": payees})
}

// paymentOptionsClaim 要求整单读取及明确财务动作，普通付款仍限定待付款状态。
// 参数：db/a 为数据库和身份；claimID 为当前单据。
// 返回值：已授权整单；无权限、自付或普通付款状态冲突时拒绝。
func (s *Service) paymentOptionsClaim(db *gorm.DB, a domain.Actor, claimID string) (domain.Claim, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return domain.Claim{}, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	permission := "expense.payment.record"
	if a.Has("expense.payment.exception.record") {
		permission = "expense.payment.exception.record"
	}
	c, _, err := loadClaim(db, a.TenantID, claimID, false)
	if err != nil {
		return c, err
	}
	if err = requireFinancial(db, a, c, permission); err != nil {
		return c, err
	}
	full, err := s.fullRead(db, a, c)
	if err != nil || !full {
		if err == nil {
			err = domain.Fail("RESOURCE_NOT_FOUND", "没有完整单据访问权限", 404)
		}
		return c, err
	}
	if permission == "expense.payment.record" {
		err = validateOrdinaryPaymentSelector(db, a, c)
	}
	return c, err
}

// validateOrdinaryPaymentSelector 将普通付款选项限定为当前状态和独立出纳。
// 参数：db/a 为数据库及选择人；c 为已确认财务范围的单据。
// 返回值：状态、自付或会计出纳职责冲突时拒绝，其余返回无错误。
func validateOrdinaryPaymentSelector(db *gorm.DB, a domain.Actor, c domain.Claim) error {
	if c.Status != "APPROVED_PENDING_PAYMENT" || c.ActivePaymentID != "" || c.Applicant == a.SubjectID {
		return domain.Fail("STATE_CONFLICT", "当前单据不能由此人员登记普通付款", 409)
	}
	var count int64
	err := db.Model(&store.Action{}).
		Joins("JOIN exp_approval_task ON exp_approval_task.id = exp_approval_action.task_id AND exp_approval_task.tenant_id = exp_approval_action.tenant_id").
		Where("exp_approval_action.tenant_id = ? AND exp_approval_action.claim_id = ? AND exp_approval_action.round = ? AND exp_approval_action.actor = ? AND exp_approval_action.action = ? AND exp_approval_task.node_code = ?",
			c.TenantID, c.ID, c.CurrentRound, a.SubjectID, "APPROVE", "ACCOUNTANT").Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return domain.Fail("ACCOUNTANT_CASHIER_CONFLICT", "会计审核人与付款登记人必须独立", 403)
	}
	return nil
}

// paymentPayeeChoices 列出申请人当前已发布、启用并审核的收款资料，保留实际付款选择。
// 参数：db 为数据库；c 为已授权单据，申请人由服务端派生。
// 返回值：安全受控引用及配置版本，不将实际异常资料强制改为批准档案。
func paymentPayeeChoices(db *gorm.DB, c domain.Claim) ([]map[string]any, error) {
	rows, err := configs(db, c.TenantID, "payee_profiles", true)
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, row := range rows {
		var profile domain.PayeeProfile
		if err = json.Unmarshal([]byte(row.ContentJSON), &profile); err != nil {
			return nil, err
		}
		if profile.Active && profile.Reviewed && profile.SubjectID == c.Applicant {
			items = append(items, map[string]any{"id": profile.ID, "name": "本人已审核收款资料",
				"account_ref": profile.AccountRef, "version": profile.Version})
		}
	}
	return items, nil
}
