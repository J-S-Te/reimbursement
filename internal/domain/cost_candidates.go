package domain

// CostItem 保存批准快照中的费用条目与分摊身份，不携带票面或账户原文。
type CostItem struct {
	OrganizationID      string `json:"organization_id"`
	Applicant           string `json:"applicant_subject_id"`
	ClaimType           string `json:"claim_type"`
	ApprovedContentHash string `json:"approved_content_hash"`
	ItemID              string `json:"item_id"`
	AllocationKey       string `json:"allocation_key"`
	ProjectID           string `json:"project_id"`
	ServiceItemID       string `json:"service_item_id,omitempty"`
	ExpenseNature       string `json:"expense_nature"`
	FeeType             string `json:"fee_type"`
	OccurredDate        string `json:"occurred_date"`
	AmountMinor         int64  `json:"amount_minor"`
	EvidenceDigest      string `json:"evidence_digest"`
}
type CostClaim struct {
	ClaimID            string        `json:"claim_id"`
	ClaimNo            string        `json:"claim_no"`
	DocumentRevision   int64         `json:"document_revision"`
	ApprovalRound      int           `json:"approval_round"`
	ProjectionRevision int64         `json:"projection_revision"`
	Currency           string        `json:"currency"`
	Applicant          string        `json:"applicant_subject_id"`
	OrganizationID     string        `json:"organization_id"`
	ClaimType          string        `json:"claim_type"`
	Status             string        `json:"status"`
	UnknownReason      string        `json:"unknown_reason,omitempty"`
	Effects            []SpendEffect `json:"effects"`
	FrozenItems        []CostItem    `json:"frozen_items"`
}
type CostSnapshot struct {
	SchemaVersion     int         `json:"schema_version"`
	SourceApplication string      `json:"source_application"`
	SourceEnvironment string      `json:"source_environment"`
	TenantID          string      `json:"tenant_id"`
	SnapshotAt        string      `json:"snapshot_at"`
	Complete          bool        `json:"complete"`
	Claims            []CostClaim `json:"claims"`
}
