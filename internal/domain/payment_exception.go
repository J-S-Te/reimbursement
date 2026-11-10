package domain

import "time"

type PaymentExceptionFact struct {
	ID                   string    `json:"id"`
	TenantID             string    `json:"tenant_id"`
	ClaimID              string    `json:"claim_id"`
	Recorder             string    `json:"recorder_subject_id"`
	ObservedClaimStatus  string    `json:"observed_claim_status"`
	ObservedClaimVersion int64     `json:"observed_claim_version"`
	Amount               Money     `json:"amount_minor"`
	Currency             string    `json:"currency"`
	PaidDate             string    `json:"paid_date"`
	Method               string    `json:"method"`
	SourceAccountRef     string    `json:"source_account_ref"`
	TransactionNo        string    `json:"transaction_no"`
	PayeeProfileID       string    `json:"payee_profile_id,omitempty"`
	VoucherFileIDs       []string  `json:"voucher_file_ids"`
	Reason               string    `json:"reason"`
	CreatedAt            time.Time `json:"created_at"`
}

type PaymentExceptionReview struct {
	ID               string    `json:"id"`
	ExceptionID      string    `json:"exception_id"`
	Actor            string    `json:"actor_subject_id"`
	Decision         string    `json:"decision"`
	Reason           string    `json:"reason"`
	EvidenceFileIDs  []string  `json:"evidence_file_ids"`
	LinkedPaymentID  string    `json:"linked_payment_id,omitempty"`
	LinkedReversalID string    `json:"linked_reversal_id,omitempty"`
	PreviousStatus   string    `json:"previous_status"`
	Status           string    `json:"status"`
	PreviousVersion  int64     `json:"previous_version"`
	Version          int64     `json:"version"`
	ClaimVersion     int64     `json:"claim_version"`
	ClaimStatus      string    `json:"claim_status"`
	CreatedAt        time.Time `json:"created_at"`
}
