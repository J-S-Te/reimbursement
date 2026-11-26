package domain

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

type Money int64

// MarshalJSON 将整数分编码为十进制字符串。
// 参数：无。
// 返回值：金额 JSON 与编码错误。
func (m Money) MarshalJSON() ([]byte, error) { return json.Marshal(strconv.FormatInt(int64(m), 10)) }

// UnmarshalJSON 仅接受精确整数分字符串。
// 参数：data 为客户端金额 JSON。
// 返回值：格式或溢出错误。
func (m *Money) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil && strconv.FormatInt(n, 10) != v {
		err = Fail("INVALID_AMOUNT", "金额必须为规范十进制整数字符串", 422)
	}
	if err == nil {
		*m = Money(n)
	}
	return err
}

type Actor struct {
	RequestID   string   `json:"-"`
	TenantID    string   `json:"tenant_id"`
	SubjectID   string   `json:"subject_id"`
	Permissions []string `json:"permissions"`
	OrgIDs      []string `json:"org_ids"`
}
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type Problem struct {
	FieldErrors []FieldError `json:"field_errors,omitempty"`
	Code        string       `json:"code"`
	Message     string       `json:"message"`
	Status      int          `json:"-"`
}

// Error 返回适合公开的业务错误。
// 参数：无。
// 返回值：中文错误摘要。
func (p *Problem) Error() string { return p.Message }

// Fail 构造带 HTTP 语义的领域错误。
// 参数：code 为稳定错误码，message 为公开说明，status 为状态码。
// 返回值：业务错误。
func Fail(code, message string, status int) error {
	return &Problem{Code: code, Message: message, Status: status}
}

// Has 检查精确权限码，管理角色不隐式扩权。
// 参数：permission 为需要的动作权限。
// 返回值：是否拥有该精确权限。
func (a Actor) Has(permission string) bool {
	for _, p := range a.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

type Allocation struct {
	ProjectID        string `json:"project_id"`
	ServiceItemID    string `json:"service_item_id,omitempty"`
	Amount           Money  `json:"amount_minor"`
	RatioBasisPoints int64  `json:"ratio_basis_points,omitempty"`
}
type InvoiceUsage struct {
	InvoiceID string `json:"invoice_id"`
	Used      Money  `json:"used_minor"`
}
type Item struct {
	ClassificationEvidence        []InvoiceClassificationEvidence `json:"classification_evidence,omitempty"`
	ExpenseNature                 string                          `json:"expense_nature,omitempty"`
	FinanceReviewRequired         bool                            `json:"finance_review_required,omitempty"`
	ReviewHints                   []string                        `json:"review_hints,omitempty"`
	TravelDeclaration             *TravelDeclaration              `json:"travel_declaration,omitempty"`
	AllocationMode                string                          `json:"allocation_mode,omitempty"`
	AllocationRuleVersion         string                          `json:"allocation_rule_version,omitempty"`
	StandardPolicyVersion         int64                           `json:"standard_policy_version,omitempty"`
	TransportCategory             string                          `json:"transport_category,omitempty"`
	AccommodationCategory         string                          `json:"accommodation_category,omitempty"`
	CityCode                      string                          `json:"city_code,omitempty"`
	NoInvoiceProofType            string                          `json:"no_invoice_proof_type,omitempty"`
	NoInvoiceEvidenceFileIDs      []string                        `json:"no_invoice_evidence_file_ids,omitempty"`
	PolicyCompatibility           []string                        `json:"policy_compatibility,omitempty"`
	HistoricalTailReason          string                          `json:"historical_tail_reason,omitempty"`
	HistoricalTailEvidenceFileIDs []string                        `json:"historical_tail_evidence_file_ids,omitempty"`
	HistoricalTailApplications    []HistoricalTailApplication     `json:"historical_tail_applications,omitempty"`
	TravelConfirmation            *TravelConfirmation             `json:"travel_confirmation,omitempty"`
	TravelAssessment              *TravelAssessment               `json:"travel_assessment,omitempty"`
	RequiresTravelFacts           bool                            `json:"requires_travel_facts,omitempty"`
	ID                            string                          `json:"id"`
	FeeType                       string                          `json:"fee_type"`
	OccurredDate                  string                          `json:"occurred_date"`
	Requested                     Money                           `json:"requested_minor"`
	Reason                        string                          `json:"reason"`
	ExceptionReason               string                          `json:"exception_reason"`
	TravelException               bool                            `json:"travel_exception"`
	CityLevel                     string                          `json:"city_level"`
	Grade                         string                          `json:"grade"`
	Units                         int64                           `json:"units"`
	NoInvoiceReason               string                          `json:"no_invoice_reason"`
	Allocations                   []Allocation                    `json:"allocations"`
	InvoiceUsages                 []InvoiceUsage                  `json:"invoice_usages"`
	StandardVersionID             string                          `json:"standard_version_id,omitempty"`
	StandardLimit                 Money                           `json:"standard_limit_minor"`
	OverStandard                  bool                            `json:"over_standard"`
}
type Claim struct {
	ClaimType             string                    `json:"claim_type,omitempty"`
	Remark                string                    `json:"remark,omitempty"`
	InvoiceDateWarnings   []InvoiceDateWarning      `json:"invoice_date_warnings,omitempty"`
	ArchiveRetention      *ArchiveRetentionSnapshot `json:"archive_retention,omitempty"`
	FinanceEntitySnapshot *FinanceEntity            `json:"finance_entity_snapshot,omitempty"`
	PayeeProfileSnapshot  *PayeeProfile             `json:"payee_profile_snapshot,omitempty"`
	ID                    string                    `json:"id"`
	TenantID              string                    `json:"tenant_id"`
	ClaimNo               string                    `json:"claim_no"`
	Title                 string                    `json:"title"`
	Applicant             string                    `json:"applicant_subject_id"`
	ApplicantName         string                    `json:"applicant_name,omitempty"`
	OrgID                 string                    `json:"org_id"`
	RouteType             string                    `json:"route_type"`
	Status                string                    `json:"status"`
	Version               int64                     `json:"version"`
	CurrentRound          int                       `json:"current_round"`
	Currency              string                    `json:"currency"`
	Payable               Money                     `json:"payable_minor"`
	FinanceEntityID       string                    `json:"finance_entity_id"`
	PayeeProfileID        string                    `json:"payee_profile_id"`
	PayeeVersion          int64                     `json:"payee_version"`
	Items                 []Item                    `json:"items"`
	ContentHash           string                    `json:"content_hash,omitempty"`
	ActivePaymentID       string                    `json:"active_payment_id,omitempty"`
	ProjectionRevision    int64                     `json:"projection_revision"`
	CreatedAt             time.Time                 `json:"created_at"`
	UpdatedAt             time.Time                 `json:"updated_at"`
}
type Invoice struct {
	Content             *InvoiceContent     `json:"content,omitempty"`
	TravelFacts         *InvoiceTravelFacts `json:"travel_facts,omitempty"`
	MergedIntoInvoiceID string              `json:"merged_into_invoice_id,omitempty"`
	ID                  string              `json:"id"`
	TenantID            string              `json:"tenant_id"`
	Owner               string              `json:"owner_subject_id"`
	Version             int64               `json:"version"`
	IdentityKey         string              `json:"identity_key"`
	InvoiceType         string              `json:"invoice_type"`
	InvoiceNumber       string              `json:"invoice_number"`
	InvoiceCode         string              `json:"invoice_code"`
	InvoiceDate         string              `json:"invoice_date"`
	SellerTaxID         string              `json:"seller_tax_id"`
	BuyerTaxID          string              `json:"buyer_tax_id"`
	Total               Money               `json:"total_minor"`
	ReviewStatus        string              `json:"review_status"`
	VerificationStatus  string              `json:"verification_status"`
	FileIDs             []string            `json:"file_ids"`
	ReservationClaimID  string              `json:"reservation_claim_id,omitempty"`
	ReservationState    string              `json:"reservation_state"`
}
type Standard struct {
	AllowPrepayment       bool     `json:"allow_prepayment,omitempty"`
	TransportCategory     string   `json:"transport_category,omitempty"`
	AccommodationCategory string   `json:"accommodation_category,omitempty"`
	RequireNoInvoiceProof bool     `json:"require_no_invoice_proof,omitempty"`
	NoInvoiceProofTypes   []string `json:"no_invoice_proof_types,omitempty"`
	RequiresTravelFacts   bool     `json:"requires_travel_facts,omitempty"`
	ID                    string   `json:"id"`
	Version               int64    `json:"version"`
	FeeType               string   `json:"fee_type"`
	OrgID                 string   `json:"org_id"`
	CityLevel             string   `json:"city_level"`
	Grade                 string   `json:"grade"`
	Unit                  string   `json:"unit"`
	Limit                 Money    `json:"limit_minor"`
	ValidFrom             string   `json:"valid_from"`
	ValidUntil            string   `json:"valid_until"`
	Priority              int      `json:"priority"`
	AllowNoInvoice        bool     `json:"allow_no_invoice"`
	Published             bool     `json:"published"`
}
type Assignment struct {
	ID         string `json:"id"`
	Version    int64  `json:"version"`
	OrgID      string `json:"org_id"`
	ProjectID  string `json:"project_id,omitempty"`
	Role       string `json:"role"`
	SubjectID  string `json:"subject_id"`
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
	Published  bool   `json:"published"`
}
type FinanceEntity struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	TaxID  string   `json:"tax_id"`
	OrgIDs []string `json:"org_ids"`
	Active bool     `json:"active"`
}
type PayeeProfile struct {
	ID         string `json:"id"`
	SubjectID  string `json:"subject_id"`
	AccountRef string `json:"account_ref"`
	Version    int64  `json:"version"`
	Active     bool   `json:"active"`
	Reviewed   bool   `json:"reviewed"`
}
type Scope struct {
	ID        string   `json:"id"`
	SubjectID string   `json:"subject_id"`
	OrgIDs    []string `json:"org_ids"`
	TenantAll bool     `json:"tenant_all"`
	Published bool     `json:"published"`
	Version   int64    `json:"version"`
}
type ApproverRef struct {
	NodeRole      string `json:"node_role,omitempty"`
	SourceDigest  string `json:"source_digest,omitempty"`
	SourceOrgID   string `json:"source_org_id,omitempty"`
	SubjectID     string `json:"subject_id"`
	SourceField   string `json:"source_field"`
	SourceVersion uint64 `json:"source_version"`
}
type ReferenceRequest struct {
	ApprovalOrgID string `json:"approval_org_id,omitempty"`
	ProjectID     string `json:"project_id"`
	ServiceItemID string `json:"service_item_id,omitempty"`
	OccurredDate  string `json:"occurred_date"`
}
type ProjectReference struct {
	ApproverReasonCodes  map[string][]string `json:"approver_reason_codes,omitempty"`
	ParticipantEligible  bool                `json:"participant_eligible,omitempty"`
	ProjectStatus        string              `json:"project_status,omitempty"`
	ServiceItemStatus    string              `json:"service_item_status,omitempty"`
	TravelSnapshot       *TravelSnapshot     `json:"travel_snapshot,omitempty"`
	ProjectID            string              `json:"project_id"`
	ProjectName          string              `json:"project_name"`
	SourceKind           string              `json:"source_kind"`
	ContractID           string              `json:"contract_id,omitempty"`
	CustomerID           string              `json:"customer_id,omitempty"`
	ServiceItemVersion   uint64              `json:"service_item_version,omitempty"`
	Site                 string              `json:"site,omitempty"`
	PlannedStart         string              `json:"planned_start,omitempty"`
	PlannedEnd           string              `json:"planned_end,omitempty"`
	ServiceItemID        string              `json:"service_item_id,omitempty"`
	Eligible             bool                `json:"eligible"`
	EligibilitySource    string              `json:"eligibility_source"`
	RequiredApproverRefs []ApproverRef       `json:"required_approver_refs"`
	ReferenceVersion     string              `json:"reference_version"`
	ProjectVersion       uint64              `json:"project_version"`
	ValidatedAt          time.Time           `json:"validated_at"`
	ValidUntil           time.Time           `json:"valid_until"`
	ReasonCodes          []string            `json:"reason_codes"`
}
type Task struct {
	NodeRole           string   `json:"node_role,omitempty"`
	ID                 string   `json:"id"`
	ClaimID            string   `json:"claim_id"`
	Round              int      `json:"round"`
	Node               int      `json:"node"`
	NodeCode           string   `json:"node_code"`
	Assignee           string   `json:"assignee_subject_id"`
	Status             string   `json:"status"`
	Version            int64    `json:"version"`
	AllocationKeys     []string `json:"allocation_keys"`
	RequiredPermission string   `json:"required_permission"`
}
type EffectAllocation struct {
	ProjectID     string `json:"project_id"`
	ServiceItemID string `json:"service_item_id,omitempty"`
	FeeType       string `json:"fee_type"`
	Amount        Money  `json:"amount_minor"`
}
type Effect struct {
	ID                  string             `json:"effect_id"`
	Kind                string             `json:"kind"`
	PaymentID           string             `json:"payment_record_id"`
	ReversesEffectID    string             `json:"reverses_effect_id,omitempty"`
	Amount              Money              `json:"amount_minor"`
	Currency            string             `json:"currency"`
	OccurredAt          string             `json:"occurred_at"`
	RecordedAt          time.Time          `json:"recorded_at"`
	ApprovedContentHash string             `json:"approved_content_hash"`
	Allocations         []EffectAllocation `json:"allocations"`
}

// ValidateClaim 规范化分摊并校验草稿的金额、日期与覆盖不变量。
// 参数：c 为申请人编辑的完整草稿。
// 返回值：校验错误，合法时为空。
func ValidateClaim(c *Claim) error {
	if e := ValidateResourceLimits(*c); e != nil {
		return e
	}
	if strings.TrimSpace(c.Title) == "" || len(c.Title) > 200 || c.Currency != "CNY" || (c.RouteType != "TECHNICAL" && c.RouteType != "SALES") || len(c.Items) == 0 || len(c.Items) > 100 {
		return Fail("INVALID_CLAIM", "请填写标题、CNY币种及技术或销售线路的费用项", 422)
	}
	total := Money(0)
	itemIDs := map[string]bool{}
	for i := range c.Items {
		if e := NormalizeItemAllocations(&c.Items[i]); e != nil {
			return e
		}
		item := c.Items[i]
		if item.ID == "" || itemIDs[item.ID] || item.FeeType == "" || item.Requested <= 0 || item.Requested > Money(math.MaxInt64)-total {
			return Fail("INVALID_ITEM", "费用项标识、类型或金额不合法", 422)
		}
		itemIDs[item.ID] = true
		if _, err := time.Parse("2006-01-02", item.OccurredDate); err != nil {
			return Fail("INVALID_DATE", "费用日期必须为有效业务日期", 422)
		}
		allocated := Money(0)
		keys := map[[2]string]bool{}
		for _, a := range item.Allocations {
			key := [2]string{a.ProjectID, a.ServiceItemID}
			if a.ProjectID == "" || keys[key] || a.Amount <= 0 || a.Amount > item.Requested-allocated {
				return Fail("ALLOCATION_MISMATCH", "项目分摊必须为正数且不得重复或超过费用金额", 422)
			}
			keys[key] = true
			allocated += a.Amount
		}
		if allocated != item.Requested {
			return Fail("ALLOCATION_MISMATCH", "项目分摊合计必须等于费用金额", 422)
		}
		covered := Money(0)
		invoices := map[string]bool{}
		for _, u := range item.InvoiceUsages {
			if u.InvoiceID == "" || invoices[u.InvoiceID] || u.Used <= 0 || u.Used > item.Requested-covered {
				return Fail("INVOICE_COVERAGE_MISMATCH", "发票使用关系重复或金额不合法", 422)
			}
			invoices[u.InvoiceID] = true
			covered += u.Used
		}
		if covered != item.Requested && !(covered == 0 && strings.TrimSpace(item.NoInvoiceReason) != "") {
			return Fail("INVOICE_COVERAGE_MISMATCH", "费用必须由发票全额覆盖或按明确政策申请无票费用", 422)
		}
		total += item.Requested
	}
	c.Payable = total
	return nil
}

type EmployeeProfile struct {
	ID        string `json:"id"`
	SubjectID string `json:"subject_id"`
	OrgID     string `json:"org_id"`
	Grade     string `json:"grade"`
	RouteType string `json:"route_type"`
	Version   int64  `json:"version"`
	Active    bool   `json:"active"`
}
type InvoiceRule struct {
	ID                           string   `json:"id"`
	InvoiceType                  string   `json:"invoice_type"`
	IdentityFields               []string `json:"identity_fields"`
	RequiredBuyerTaxID           bool     `json:"required_buyer_tax_id"`
	RequiredExternalVerification bool     `json:"required_external_verification"`
	Version                      int64    `json:"version"`
	Active                       bool     `json:"active"`
}

type RouteRule struct {
	ExtraThresholdBasisPoints int64  `json:"extra_threshold_basis_points"`
	ExtraThresholdMinor       Money  `json:"extra_threshold_minor"`
	ID                        string `json:"id"`
	Version                   int64  `json:"version"`
	RouteType                 string `json:"route_type"`
	OrgID                     string `json:"org_id"`
	ValidFrom                 string `json:"valid_from"`
	ValidUntil                string `json:"valid_until"`
	BaseRole                  string `json:"base_role"`
	ExtraRole                 string `json:"extra_role"`
	AccountingRole            string `json:"accounting_role"`
	Published                 bool   `json:"published"`
}

type TransferQualification struct {
	TenantAll          bool      `json:"tenant_all,omitempty"`
	TargetSubjectID    string    `json:"target_subject_id"`
	RequiredPermission string    `json:"required_permission"`
	VerifiedAt         time.Time `json:"verified_at"`
	TaskID             string    `json:"task_id"`
	TaskVersion        int64     `json:"task_version"`
	ClaimVersion       int64     `json:"claim_version"`
	Round              int       `json:"round"`
	Source             string    `json:"source"`
}

type ApproverQualification struct {
	SubjectID          string    `json:"subject_id"`
	RequiredPermission string    `json:"required_permission"`
	VerifiedAt         time.Time `json:"verified_at"`
	Source             string    `json:"source"`
}

// ValidateResourceLimits 在调用任何远端依赖前校验单据的有限资源形状。
// 参数：c 为允许保存不完整内容但准备提交的草稿。
// 返回值：费用项、不同票据或分摊总行超限错误，不截断原草稿。
func ValidateResourceLimits(c Claim) error {
	if len(c.Items) > 100 {
		return Fail("CLAIM_RESOURCE_LIMIT", "费用项最多100条", 422)
	}
	invoiceSet := map[string]bool{}
	evidenceSet := map[string]bool{}
	allocations := 0
	for _, item := range c.Items {
		if len(item.HistoricalTailEvidenceFileIDs) > 20 {
			return Fail("CLAIM_RESOURCE_LIMIT", "单项尾款证据最多20个", 422)
		}
		for _, id := range item.HistoricalTailEvidenceFileIDs {
			if id == "" {
				return Fail("CLAIM_RESOURCE_LIMIT", "证据引用不能为空", 422)
			}
			evidenceSet[id] = true
		}
		if len(item.NoInvoiceEvidenceFileIDs) > 20 {
			return Fail("CLAIM_RESOURCE_LIMIT", "单项无票证据最多20个", 422)
		}
		for _, id := range item.NoInvoiceEvidenceFileIDs {
			if strings.TrimSpace(id) == "" {
				return Fail("CLAIM_RESOURCE_LIMIT", "无票证据引用不能为空", 422)
			}
			evidenceSet[id] = true
		}
		if item.TravelConfirmation != nil {
			if len(item.TravelConfirmation.EvidenceFileIDs) > 20 {
				return Fail("CLAIM_RESOURCE_LIMIT", "单项行程证据最多20个", 422)
			}
			for _, id := range item.TravelConfirmation.EvidenceFileIDs {
				if id == "" {
					return Fail("CLAIM_RESOURCE_LIMIT", "证据引用不能为空", 422)
				}
				evidenceSet[id] = true
			}
		}
		if len(evidenceSet) > 200 {
			return Fail("CLAIM_RESOURCE_LIMIT", "整单不同费用证据最多200个", 422)
		}
		allocations += len(item.Allocations)
		if allocations > 1000 {
			return Fail("CLAIM_RESOURCE_LIMIT", "单据项目分摊最多1000行", 422)
		}
		for _, usage := range item.InvoiceUsages {
			invoiceSet[usage.InvoiceID] = true
			if len(invoiceSet) > 200 {
				return Fail("CLAIM_RESOURCE_LIMIT", "单据不同发票最多200张", 422)
			}
		}
	}
	return nil
}
