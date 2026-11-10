package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrSpendProtocol = errors.New("reimbursement spend protocol conflict")

// 完整不可变历史允许旧版合法付款追加一次完整冲正；新付款仍须先预留反向空间。
const (
	SpendSnapshotMaxBytes         = 2 << 20
	SpendSnapshotMaxAllocations   = 2000
	SpendSnapshotMaxEffects       = 2000
	SpendSnapshotMaxContributions = 1000
)

type SpendAllocation struct {
	ProjectID     string  `json:"project_id"`
	ServiceItemID *string `json:"service_item_id"`
	FeeType       string  `json:"fee_type"`
	AmountMinor   string  `json:"amount_minor"`
}
type SpendEffect struct {
	EffectID            string            `json:"effect_id"`
	PaymentRecordID     string            `json:"payment_record_id"`
	Kind                string            `json:"kind"`
	ReversesEffectID    string            `json:"reverses_effect_id,omitempty"`
	AmountMinor         string            `json:"amount_minor"`
	OccurredAt          string            `json:"occurred_at"`
	RecordedAt          string            `json:"recorded_at"`
	ApprovedContentHash string            `json:"approved_content_hash"`
	Allocations         []SpendAllocation `json:"allocations"`
}
type SpendContribution struct {
	ProjectID     string  `json:"project_id"`
	ServiceItemID *string `json:"service_item_id"`
	FeeType       string  `json:"fee_type"`
	PaidNetMinor  string  `json:"paid_net_minor"`
}
type SpendSnapshot struct {
	SchemaVersion      int                 `json:"schema_version"`
	EventID            string              `json:"event_id,omitempty"`
	SourceApplication  string              `json:"source_application"`
	SourceEnvironment  string              `json:"source_environment"`
	TenantID           string              `json:"tenant_id"`
	ClaimID            string              `json:"claim_id"`
	ClaimNo            string              `json:"claim_no"`
	DocumentRevision   uint64              `json:"document_revision"`
	ApprovalRound      uint64              `json:"approval_round"`
	ProjectionRevision uint64              `json:"projection_revision"`
	CompleteSnapshot   bool                `json:"complete_snapshot"`
	Trigger            string              `json:"trigger"`
	Currency           string              `json:"currency"`
	SnapshotHash       string              `json:"snapshot_hash,omitempty"`
	Effects            []SpendEffect       `json:"effects"`
	Contributions      []SpendContribution `json:"contributions"`
}
type SpendReceipt struct {
	Status           string `json:"status"`
	ReceivedRevision uint64 `json:"received_revision"`
	AppliedRevision  uint64 `json:"applied_revision"`
	SnapshotHash     string `json:"snapshot_hash"`
}

// SpendKey 返回项目、可选服务项和费用类型组成的无歧义分组键。
// 参数：project 为项目标识，item 为可选服务项，fee 为费用类型。
// 返回值：使用 NUL 分隔的稳定分组键。
func SpendKey(project string, item *string, fee string) string {
	id := ""
	if item != nil {
		id = *item
	}
	return project + "\x00" + id + "\x00" + fee
}

// CanonicalSpendSnapshot 固定字段、排序和空服务项表示以生成规范摘要载荷。
// 参数：s 为来源完整快照。
// 返回值：排除 event_id 和 snapshot_hash 的规范 JSON，序列化失败返回错误。
func CanonicalSpendSnapshot(s SpendSnapshot) ([]byte, error) {
	s.EventID = ""
	s.SnapshotHash = ""
	s.Effects = append([]SpendEffect{}, s.Effects...)
	s.Contributions = append([]SpendContribution{}, s.Contributions...)
	for i := range s.Effects {
		s.Effects[i].Allocations = append([]SpendAllocation{}, s.Effects[i].Allocations...)
		sort.Slice(s.Effects[i].Allocations, func(a, b int) bool {
			x, y := s.Effects[i].Allocations[a], s.Effects[i].Allocations[b]
			return SpendKey(x.ProjectID, x.ServiceItemID, x.FeeType) < SpendKey(y.ProjectID, y.ServiceItemID, y.FeeType)
		})
	}
	sort.Slice(s.Effects, func(i, j int) bool { return s.Effects[i].EffectID < s.Effects[j].EffectID })
	sort.Slice(s.Contributions, func(i, j int) bool {
		x, y := s.Contributions[i], s.Contributions[j]
		return SpendKey(x.ProjectID, x.ServiceItemID, x.FeeType) < SpendKey(y.ProjectID, y.ServiceItemID, y.FeeType)
	})
	return json.Marshal(s)
}

// SpendSnapshotHash 计算跨来源与目标共用的规范 SHA256 摘要。
// 参数：s 为完整快照。
// 返回值：小写十六进制摘要，无法序列化时返回错误。
func SpendSnapshotHash(s SpendSnapshot) (string, error) {
	b, e := CanonicalSpendSnapshot(s)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// SpendEffectHash 计算不可变效果含逐项分摊的内容摘要。
// 参数：effect 为付款或全额冲正效果。
// 返回值：规范内容的 SHA256，用于效果主键与业务键冲突检测。
func SpendEffectHash(effect SpendEffect) string {
	b, _ := CanonicalSpendSnapshot(SpendSnapshot{Effects: []SpendEffect{effect}})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// spendAmount 读取规范十进制整数并限制在持久化 BIGINT 范围内。
// 参数：raw 为 JSON 金额字符串。
// 返回值：精确整数，非规范或溢出时返回协议错误。
func spendAmount(raw string) (*big.Int, error) {
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || strconv.FormatInt(n, 10) != raw {
		return nil, ErrSpendProtocol
	}
	return big.NewInt(n), nil
}

// spendIdentifier 校验协议标识以避免空白、分隔符和超长值。
// 参数：s 为输入标识。
// 返回值：是否为非空、无控制字符且不超过128字节的标识。
func spendIdentifier(s string) bool {
	if s == "" || len(s) > 128 || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

// ValidateSpendSnapshot 验证完整历史效果、全额冲正、逐项分摊和当前贡献相等。
// 参数：s 为接收的冻结快照。
// 返回值：合法返回 nil；金额、摘要、限制或来源形状错误返回协议错误。
func ValidateSpendSnapshot(s SpendSnapshot) error {
	b, e := json.Marshal(s)
	if e != nil || len(b) > SpendSnapshotMaxBytes || s.SchemaVersion != 1 || !s.CompleteSnapshot || s.SourceApplication != "reimbursement" || !spendIdentifier(s.EventID) || !spendIdentifier(s.TenantID) || !spendIdentifier(s.SourceEnvironment) || !spendIdentifier(s.ClaimID) || !spendIdentifier(s.ClaimNo) || s.DocumentRevision == 0 || s.ApprovalRound == 0 || s.ProjectionRevision == 0 || s.Currency != "CNY" || (s.Trigger != "PAYMENT_RECORDED" && s.Trigger != "PAYMENT_FULLY_REVERSED") || len(s.Effects) == 0 || len(s.Effects) > SpendSnapshotMaxEffects || len(s.Contributions) == 0 || len(s.Contributions) > SpendSnapshotMaxContributions {
		return ErrSpendProtocol
	}
	h, _ := SpendSnapshotHash(s)
	if h != s.SnapshotHash {
		return ErrSpendProtocol
	}
	effects := map[string]SpendEffect{}
	business := map[string]bool{}
	net := map[string]*big.Int{}
	allocationCount := 0
	for _, effect := range s.Effects {
		if !spendIdentifier(effect.EffectID) || !spendIdentifier(effect.PaymentRecordID) || effects[effect.EffectID].EffectID != "" || business[effect.PaymentRecordID+"\x00"+effect.Kind] {
			return ErrSpendProtocol
		}
		amount, err := spendAmount(effect.AmountMinor)
		if err != nil || amount.Sign() == 0 {
			return ErrSpendProtocol
		}
		if effect.Kind == "PAYMENT_RECORDED" {
			if amount.Sign() < 0 || effect.ReversesEffectID != "" {
				return ErrSpendProtocol
			}
		} else if effect.Kind != "PAYMENT_FULLY_REVERSED" || amount.Sign() > 0 || !spendIdentifier(effect.ReversesEffectID) {
			return ErrSpendProtocol
		}
		for _, raw := range []string{effect.OccurredAt, effect.RecordedAt} {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil || at.UTC().Format(time.RFC3339) != raw {
				return ErrSpendProtocol
			}
		}
		hash, err := hex.DecodeString(effect.ApprovedContentHash)
		if err != nil || len(hash) != 32 || strings.ToLower(effect.ApprovedContentHash) != effect.ApprovedContentHash {
			return ErrSpendProtocol
		}
		sum := new(big.Int)
		seen := map[string]bool{}
		if len(effect.Allocations) == 0 {
			return ErrSpendProtocol
		}
		for _, a := range effect.Allocations {
			allocationCount++
			key := SpendKey(a.ProjectID, a.ServiceItemID, a.FeeType)
			v, err := spendAmount(a.AmountMinor)
			if err != nil || v.Sign() != amount.Sign() || !spendIdentifier(a.ProjectID) || !spendIdentifier(a.FeeType) || (a.ServiceItemID != nil && !spendIdentifier(*a.ServiceItemID)) || seen[key] {
				return ErrSpendProtocol
			}
			seen[key] = true
			sum.Add(sum, v)
			if net[key] == nil {
				net[key] = new(big.Int)
			}
			net[key].Add(net[key], v)
		}
		if sum.Cmp(amount) != 0 || allocationCount > SpendSnapshotMaxAllocations {
			return ErrSpendProtocol
		}
		effects[effect.EffectID] = effect
		business[effect.PaymentRecordID+"\x00"+effect.Kind] = true
	}
	reversed := map[string]bool{}
	for _, effect := range s.Effects {
		if effect.Kind == "PAYMENT_FULLY_REVERSED" {
			if err := validateSpendReversal(effect, effects, reversed); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Contributions {
		key := SpendKey(c.ProjectID, c.ServiceItemID, c.FeeType)
		v, err := spendAmount(c.PaidNetMinor)
		if err != nil || v.Sign() < 0 || seen[key] || net[key] == nil || v.Cmp(net[key]) != 0 {
			return ErrSpendProtocol
		}
		seen[key] = true
	}
	if len(seen) != len(net) {
		return ErrSpendProtocol
	}
	return nil
}

// validateSpendReversal 验证一笔付款最多一次全额反向且逐行对应原付款。
// 参数：reverse 为冲正效果，effects 为完整效果集合，seen 为已冲正付款集合。
// 返回值：合法返回 nil，原付款缺失或反向明细不符返回协议错误。
func validateSpendReversal(reverse SpendEffect, effects map[string]SpendEffect, seen map[string]bool) error {
	original, ok := effects[reverse.ReversesEffectID]
	if !ok || original.Kind != "PAYMENT_RECORDED" || original.PaymentRecordID != reverse.PaymentRecordID || seen[original.EffectID] || reverse.AmountMinor != "-"+original.AmountMinor || len(reverse.Allocations) != len(original.Allocations) || reverse.ApprovedContentHash != original.ApprovedContentHash {
		return ErrSpendProtocol
	}
	amounts := map[string]string{}
	for _, a := range original.Allocations {
		amounts[SpendKey(a.ProjectID, a.ServiceItemID, a.FeeType)] = a.AmountMinor
	}
	for _, a := range reverse.Allocations {
		if a.AmountMinor != "-"+amounts[SpendKey(a.ProjectID, a.ServiceItemID, a.FeeType)] {
			return ErrSpendProtocol
		}
	}
	seen[original.EffectID] = true
	return nil
}
