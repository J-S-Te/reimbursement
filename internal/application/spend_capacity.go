package application

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// SpendCapacityResult 提供完整历史使用量及下一次付款的反向空间告警。
type SpendCapacityResult struct {
	AllocationLimit       int    `json:"allocation_limit"`
	AllocationsUsed       int    `json:"allocations_used"`
	AllocationReservation int    `json:"allocation_reservation"`
	EffectLimit           int    `json:"effect_limit"`
	EffectsUsed           int    `json:"effects_used"`
	ByteLimit             int    `json:"byte_limit"`
	ReservedSnapshotBytes int    `json:"reserved_snapshot_bytes"`
	CanRecord             bool   `json:"can_record"`
	Warning               string `json:"warning,omitempty"`
}

// readSpendEffects 有界读取完整不可变历史，禁止静默截断投影。
// 参数：tx 为单据事务，c 为包含租户和标识的单据。
// 返回值：完整效果列表，超出受控边界或读取失败返回错误。
func readSpendEffects(tx *gorm.DB, c domain.Claim) ([]domain.SpendEffect, error) {
	var rows []store.Effect
	err := tx.Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Order("id").
		Limit(domain.SpendSnapshotMaxEffects + 1).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) > domain.SpendSnapshotMaxEffects {
		return nil, spendCapacityConflict()
	}
	effects := make([]domain.SpendEffect, 0, len(rows))
	for _, row := range rows {
		var effect domain.SpendEffect
		if err = json.Unmarshal([]byte(row.ContentJSON), &effect); err != nil {
			return nil, err
		}
		effects = append(effects, effect)
	}
	return effects, nil
}

// buildSpendSnapshot 从完整历史生成当前贡献，不修改或折叠原始效果。
// 参数：c 为文档版本，effects 为完整历史，trigger 为财务触发，event 为事件标识。
// 返回值：已计算规范摘要的快照或金额编码错误。
func (s *Service) buildSpendSnapshot(c domain.Claim, effects []domain.SpendEffect, trigger, event string) (domain.SpendSnapshot, error) {
	amounts := map[string]int64{}
	groups := map[string]domain.SpendContribution{}
	for _, effect := range effects {
		for _, a := range effect.Allocations {
			key := domain.SpendKey(a.ProjectID, a.ServiceItemID, a.FeeType)
			n, e := strconv.ParseInt(a.AmountMinor, 10, 64)
			if e != nil {
				return domain.SpendSnapshot{}, e
			}
			amounts[key] += n
			groups[key] = domain.SpendContribution{ProjectID: a.ProjectID, ServiceItemID: a.ServiceItemID, FeeType: a.FeeType}
		}
	}
	contributions := []domain.SpendContribution{}
	for key, group := range groups {
		group.PaidNetMinor = strconv.FormatInt(amounts[key], 10)
		contributions = append(contributions, group)
	}
	sort.Slice(contributions, func(i, j int) bool {
		return domain.SpendKey(contributions[i].ProjectID, contributions[i].ServiceItemID, contributions[i].FeeType) < domain.SpendKey(contributions[j].ProjectID, contributions[j].ServiceItemID, contributions[j].FeeType)
	})
	snapshot := domain.SpendSnapshot{SchemaVersion: 1, EventID: event, SourceApplication: "reimbursement",
		SourceEnvironment: s.deps.Environment, TenantID: c.TenantID, ClaimID: c.ID, ClaimNo: c.ClaimNo,
		DocumentRevision: uint64(c.Version), ApprovalRound: uint64(c.CurrentRound),
		ProjectionRevision: uint64(c.ProjectionRevision), CompleteSnapshot: true, Trigger: trigger,
		Currency: c.Currency, Effects: effects, Contributions: contributions}
	var err error
	snapshot.SnapshotHash, err = domain.SpendSnapshotHash(snapshot)
	return snapshot, err
}

// reservePaymentSpend 在新付之前完整预演付款与逐项全额反向，预留条数及字节空间。
// 参数：tx 为持有单据锁的事务，c 为待付款单据，payment 为本次候选效果。
// 返回值：容量告警；任一完整快照无法安全发布时拒绝新付。
func (s *Service) reservePaymentSpend(tx *gorm.DB, c domain.Claim, payment domain.SpendEffect) (SpendCapacityResult, error) {
	effects, err := readSpendEffects(tx, c)
	if err != nil {
		return SpendCapacityResult{}, err
	}
	return s.assessPaymentSpend(c, effects, payment)
}

// assessPaymentSpend 精确计算候选付款和未来反向的协议容量。
// 参数：c 为待付单据，effects 为历史，payment 为等长付款标识及真实明细的候选。
// 返回值：完整条数和字节告警；超限或历史不一致时返回受控错误。
func (s *Service) assessPaymentSpend(c domain.Claim, effects []domain.SpendEffect, payment domain.SpendEffect) (SpendCapacityResult, error) {
	result := spendCapacityUsage(effects)
	result.AllocationReservation = 2 * len(payment.Allocations)
	future := append(append([]domain.SpendEffect{}, effects...), payment)
	c.Version++
	c.ProjectionRevision++
	paid, err := s.buildSpendSnapshot(c, future, "PAYMENT_RECORDED", "00000000000000000000000000")
	if err != nil {
		return result, err
	}
	reverse := reservedReversal(payment)
	future = append(future, reverse)
	c.Version++
	c.ProjectionRevision++
	reversed, err := s.buildSpendSnapshot(c, future, "PAYMENT_FULLY_REVERSED", "00000000000000000000000000")
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(reversed)
	if err != nil {
		return result, err
	}
	result.ReservedSnapshotBytes = len(raw)
	if domain.ValidateSpendSnapshot(paid) != nil || domain.ValidateSpendSnapshot(reversed) != nil {
		result.Warning = "SPEND_HISTORY_CAPACITY"
		return result, spendCapacityConflict()
	}
	result.CanRecord = true
	if result.AllocationsUsed+result.AllocationReservation >= result.AllocationLimit*4/5 ||
		result.EffectsUsed+2 >= result.EffectLimit*4/5 || result.ReservedSnapshotBytes >= result.ByteLimit*4/5 {
		result.Warning = "SPEND_HISTORY_NEAR_CAPACITY"
	}
	return result, nil
}

// reservedReversal 生成与未来真实冲正等长且逐项对应的反向预演。
// 参数：payment 为当前候选付款效果。
// 返回值：不写入数据库的完整反向效果。
func reservedReversal(payment domain.SpendEffect) domain.SpendEffect {
	reverse := payment
	reverse.EffectID = "11111111111111111111111111"
	reverse.ReversesEffectID = payment.EffectID
	reverse.Kind = "PAYMENT_FULLY_REVERSED"
	reverse.AmountMinor = "-" + payment.AmountMinor
	reverse.Allocations = append([]domain.SpendAllocation{}, payment.Allocations...)
	for i := range reverse.Allocations {
		reverse.Allocations[i].AmountMinor = "-" + reverse.Allocations[i].AmountMinor
	}
	return reverse
}

// spendCapacityConflict 给出保留旧冲正能力的新付款容量拒绝原因。
// 参数：无。
// 返回值：需受控处理而不能删除历史绕过的容量业务错误。
func spendCapacityConflict() error {
	return domain.Fail("SPEND_HISTORY_CAPACITY", "完整付款历史已无法预留全额冲正空间，请联系财务处理；不得删除历史重试", 409)
}

// SpendCapacity 为当前有财务读取能力及范围的人员提供付款容量告警。
// 参数：ctx 为取消上下文，actor 为当前身份，claimID 为当前租户单据标识。
// 返回值：可付性与容量告警；越权或数据库错误返回错误。
func (s *Service) SpendCapacity(ctx context.Context, actor domain.Actor, claimID string) (SpendCapacityResult, error) {
	db := s.db.WithContext(ctx)
	c, _, err := loadClaim(db, actor.TenantID, claimID, false)
	if err != nil {
		return SpendCapacityResult{}, err
	}
	if err = requireFinancial(db, actor, c, "expense.payment.read"); err != nil {
		return SpendCapacityResult{}, err
	}
	effects, err := readSpendEffects(db, c)
	if err != nil {
		return SpendCapacityResult{}, err
	}
	if c.Status != "APPROVED_PENDING_PAYMENT" || c.ActivePaymentID != "" {
		return spendCapacityUsage(effects), nil
	}
	result, err := s.assessPaymentSpend(c, effects, paymentEffect(c, "00000000000000000000000000", "2000-01-01"))
	if err != nil && result.Warning != "SPEND_HISTORY_CAPACITY" {
		return result, err
	}
	result.CanRecord = result.CanRecord && c.Status == "APPROVED_PENDING_PAYMENT" && c.ActivePaymentID == ""
	return result, nil
}

// spendCapacityUsage 汇总不可变历史用量，不把尚未批准或已付款状态误报为容量不足。
// 参数：effects 为当前完整效果历史。
// 返回值：当前硬边界和已用量，可付款性默认关闭。
func spendCapacityUsage(effects []domain.SpendEffect) SpendCapacityResult {
	result := SpendCapacityResult{AllocationLimit: domain.SpendSnapshotMaxAllocations,
		EffectLimit: domain.SpendSnapshotMaxEffects, EffectsUsed: len(effects),
		ByteLimit: domain.SpendSnapshotMaxBytes}
	for _, effect := range effects {
		result.AllocationsUsed += len(effect.Allocations)
	}
	if result.AllocationsUsed >= result.AllocationLimit*4/5 ||
		result.EffectsUsed >= result.EffectLimit*4/5 {
		result.Warning = "SPEND_HISTORY_NEAR_CAPACITY"
	}
	return result
}
