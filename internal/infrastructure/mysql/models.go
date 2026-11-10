package mysql

import "time"

type Claim struct {
	ID              string  `gorm:"primaryKey;size:64"`
	TenantID        string  `gorm:"size:64;not null;index:idx_claim_owner,priority:1"`
	Applicant       string  `gorm:"size:128;not null;index:idx_claim_owner,priority:2"`
	OrgID           string  `gorm:"size:64;not null"`
	Status          string  `gorm:"size:40;not null;index:idx_claim_owner,priority:3"`
	Version         int64   `gorm:"not null"`
	ActivePaymentID *string `gorm:"size:64;uniqueIndex"`
	ContentJSON     string  `gorm:"type:longtext;not null"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
type Invoice struct {
	ID                 string  `gorm:"primaryKey;size:64"`
	TenantID           string  `gorm:"size:64;not null;uniqueIndex:uq_invoice_identity,priority:1"`
	Owner              string  `gorm:"size:128;not null;index"`
	IdentityKey        *string `gorm:"size:191;uniqueIndex:uq_invoice_identity,priority:2"`
	Version            int64
	ReservationClaimID string `gorm:"size:64;index"`
	ReservationState   string `gorm:"size:20;not null"`
	ContentJSON        string `gorm:"type:longtext;not null"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
type Snapshot struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;uniqueIndex:uq_claim_round,priority:1"`
	ClaimID     string `gorm:"size:64;not null;uniqueIndex:uq_claim_round,priority:2"`
	Round       int    `gorm:"not null;uniqueIndex:uq_claim_round,priority:3"`
	ContentHash string `gorm:"size:64;not null"`
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}
type Task struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;index:idx_task_inbox,priority:1"`
	ClaimID     string `gorm:"size:64;not null;index"`
	Round       int
	Node        int
	NodeCode    string `gorm:"size:40"`
	Assignee    string `gorm:"size:128;index:idx_task_inbox,priority:2"`
	Status      string `gorm:"size:20;index:idx_task_inbox,priority:3"`
	Version     int64
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
type Action struct {
	ID        string `gorm:"primaryKey;size:64"`
	TenantID  string `gorm:"size:64;not null"`
	ClaimID   string `gorm:"size:64;index"`
	TaskID    string `gorm:"size:64"`
	Actor     string `gorm:"size:128"`
	Action    string `gorm:"size:40"`
	Reason    string `gorm:"size:2000"`
	Round     int
	CreatedAt time.Time
}
type Command struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;uniqueIndex:uq_command,priority:1"`
	Actor       string `gorm:"size:128;not null;uniqueIndex:uq_command,priority:2"`
	Operation   string `gorm:"size:60;not null;uniqueIndex:uq_command,priority:3"`
	CommandID   string `gorm:"size:128;not null;uniqueIndex:uq_command,priority:4"`
	RequestHash string `gorm:"size:64;not null"`
	ResourceID  string `gorm:"size:64;not null"`
	ResultJSON  string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}
type Payment struct {
	ID               string `gorm:"primaryKey;size:64"`
	TenantID         string `gorm:"size:64;not null"`
	ClaimID          string `gorm:"size:64;not null;index"`
	Actor            string `gorm:"size:128;not null"`
	Method           string `gorm:"size:30;not null"`
	SourceAccountRef string `gorm:"size:128;not null"`
	TransactionNo    string `gorm:"size:128;not null"`
	ContentJSON      string `gorm:"type:longtext;not null"`
	CreatedAt        time.Time
}
type ReversalRequest struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null"`
	ClaimID     string `gorm:"size:64;not null;index"`
	PaymentID   string `gorm:"size:64;not null;index"`
	Requester   string `gorm:"size:128;not null"`
	Status      string `gorm:"size:20;not null"`
	Version     int64
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
type Reversal struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;uniqueIndex:uq_reversal_payment,priority:1"`
	PaymentID   string `gorm:"size:64;not null;uniqueIndex:uq_reversal_payment,priority:2"`
	ClaimID     string `gorm:"size:64;not null"`
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}
type Effect struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null"`
	ClaimID     string `gorm:"size:64;not null;index"`
	PaymentID   string `gorm:"size:64;not null;uniqueIndex:uq_effect_kind,priority:1"`
	Kind        string `gorm:"size:40;not null;uniqueIndex:uq_effect_kind,priority:2"`
	ContentHash string `gorm:"size:64;not null"`
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}
type Config struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;index:idx_config_kind,priority:1"`
	Kind        string `gorm:"size:40;not null;index:idx_config_kind,priority:2"`
	Version     int64
	Published   bool
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
type Sequence struct {
	ID    string `gorm:"primaryKey;size:128"`
	Value int64
}
type Outbox struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"size:64;not null"`
	Topic         string `gorm:"size:80;not null"`
	EventID       string `gorm:"size:64;not null;uniqueIndex"`
	ResourceID    string `gorm:"size:64;not null;index"`
	PayloadJSON   string `gorm:"type:longtext;not null"`
	Status        string `gorm:"size:20;index:idx_outbox_due,priority:1"`
	Version       int64
	Attempt       int
	LeaseOwner    string `gorm:"size:128"`
	LeaseUntil    *time.Time
	NextAttemptAt time.Time `gorm:"index:idx_outbox_due,priority:2"`
	LastError     string    `gorm:"size:1000"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
type Job struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"size:64;not null"`
	Kind          string `gorm:"size:40;not null"`
	ResourceID    string `gorm:"size:64;not null;index"`
	PayloadJSON   string `gorm:"type:longtext;not null"`
	Status        string `gorm:"size:20;index:idx_job_due,priority:1"`
	Version       int64
	Attempt       int
	LeaseOwner    string `gorm:"size:128"`
	LeaseUntil    *time.Time
	NextAttemptAt time.Time `gorm:"index:idx_job_due,priority:2"`
	LastError     string    `gorm:"size:1000"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
type Archive struct {
	ID             string `gorm:"primaryKey;size:64"`
	TenantID       string `gorm:"size:64;not null;uniqueIndex:uq_archive_version,priority:1"`
	ClaimID        string `gorm:"size:64;not null;uniqueIndex:uq_archive_version,priority:2"`
	PackageVersion int64  `gorm:"uniqueIndex:uq_archive_version,priority:3"`
	Status         string `gorm:"size:20;not null"`
	ManifestHash   string `gorm:"size:64"`
	ManifestJSON   string `gorm:"type:longtext;not null"`
	CreatedAt      time.Time
}
type Document struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null"`
	Owner       string `gorm:"size:128;not null"`
	FileID      string `gorm:"size:128"`
	Status      string `gorm:"size:30;not null"`
	Version     int64
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Models 返回报销业务模型，生产数据库仍由版本化 SQL 管理。
// 参数：无。
// 返回值：隔离测试所用模型列表。
func Models() []any {
	return []any{&ArchiveBorrow{}, &Claim{}, &Invoice{}, &Snapshot{}, &Task{}, &Action{}, &Command{}, &Payment{}, &PaymentBinding{}, &RecoveryCommand{}, &DeliveryReceipt{}, &ReconciliationCursor{}, &ReconciliationFinding{}, &ReversalRequest{}, &Reversal{}, &Effect{}, &Config{}, &Sequence{}, &Outbox{}, &Job{}, &Archive{}, &ArchiveDownload{}, &Document{}, &InvoiceRevision{}, &ClaimReference{}, &Verification{}, &PolicyLifecycle{}, &ActionDetail{}, &InvoiceParseRun{}, &InvoiceSource{}, &PaymentExceptionFact{}, &PaymentExceptionCursor{}, &PaymentExceptionReview{}, &InvoiceCorrection{}, &InvoiceControlImpact{}, &InvoiceRisk{}}
}

// TableName 指定Claim的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Claim) TableName() string { return "exp_claim" }

// TableName 指定Invoice的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Invoice) TableName() string { return "exp_invoice" }

// TableName 指定Snapshot的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Snapshot) TableName() string { return "exp_claim_snapshot" }

// TableName 指定Task的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Task) TableName() string { return "exp_approval_task" }

// TableName 指定Action的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Action) TableName() string { return "exp_approval_action" }

// TableName 指定Command的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Command) TableName() string { return "exp_command_dedup" }

// TableName 指定Payment的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Payment) TableName() string { return "exp_payment" }

// TableName 指定ReversalRequest的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (ReversalRequest) TableName() string { return "exp_payment_adjustment_request" }

// TableName 指定Reversal的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Reversal) TableName() string { return "exp_payment_reversal" }

// TableName 指定Effect的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Effect) TableName() string { return "exp_payment_effect" }

// TableName 指定Config的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Config) TableName() string { return "exp_configuration" }

// TableName 指定Sequence的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Sequence) TableName() string { return "exp_number_sequence" }

// TableName 指定Outbox的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Outbox) TableName() string { return "exp_outbox" }

// TableName 指定Job的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Job) TableName() string { return "exp_job" }

// TableName 指定Archive的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Archive) TableName() string { return "exp_archive_package" }

// TableName 指定Document的报销专用表名。
// 参数：无。
// 返回值：数据库表名。
func (Document) TableName() string { return "exp_invoice_document" }

type InvoiceRevision struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null"`
	InvoiceID   string `gorm:"size:64;not null;index"`
	Version     int64
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}

// TableName 指定人工修订不可变账本表名。
// 参数：无。
// 返回值：数据库表名。
func (InvoiceRevision) TableName() string { return "exp_invoice_field_revision" }

type ClaimReference struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"size:64;not null;index:idx_reference_project,priority:1"`
	ClaimID       string `gorm:"size:64;not null;index"`
	ProjectID     string `gorm:"size:64;not null;index:idx_reference_project,priority:2"`
	ServiceItemID string `gorm:"size:64"`
	ItemID        string `gorm:"size:64;not null"`
	Amount        int64
	CreatedAt     time.Time
}

// TableName 指定单据项目索引表名，聚合 JSON 仍是内容权威。
// 参数：无。
// 返回值：本地引用索引表名。
func (ClaimReference) TableName() string { return "exp_claim_reference" }

type Verification struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null"`
	InvoiceID   string `gorm:"size:64;not null;index"`
	Actor       string `gorm:"size:128;not null"`
	Method      string `gorm:"size:30;not null"`
	Result      string `gorm:"size:30;not null"`
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}

// TableName 指定票据核验历史表名。
// 参数：无。
// 返回值：不可变核验尝试表。
func (Verification) TableName() string { return "exp_invoice_verification_attempt" }

type PolicyLifecycle struct {
	ID              string `gorm:"primaryKey;size:64"`
	TenantID        string `gorm:"size:64;not null;uniqueIndex:uq_policy_retirement,priority:1"`
	ConfigID        string `gorm:"size:64;not null;uniqueIndex:uq_policy_retirement,priority:2"`
	Kind            string `gorm:"size:40;not null"`
	Operation       string `gorm:"size:20;not null"`
	ReplacementID   string `gorm:"size:64"`
	Actor           string `gorm:"size:128;not null"`
	Reason          string `gorm:"size:2000;not null"`
	PreviousVersion int64
	CurrentVersion  int64
	ContentHash     string `gorm:"size:64;not null"`
	EffectiveAt     time.Time
	CreatedAt       time.Time
}

// TableName 指定不可变政策退休和替换账本表。
// 参数：无。
// 返回值：报销政策生命周期表名。
func (PolicyLifecycle) TableName() string { return "exp_policy_lifecycle" }

type ActionDetail struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"size:64;not null;index"`
	ActionID      string `gorm:"size:64;not null;uniqueIndex"`
	TaskID        string `gorm:"size:64;not null;index"`
	ClaimID       string `gorm:"size:64;not null"`
	FromSubjectID string `gorm:"size:128;not null"`
	ToSubjectID   string `gorm:"size:128;not null"`
	Reason        string `gorm:"size:2000;not null"`
	ContentJSON   string `gorm:"type:longtext;not null"`
	CreatedAt     time.Time
}

// TableName 指定不可变审批责任转移明细表。
// 参数：无。
// 返回值：审批动作结构化补充账本表名。
func (ActionDetail) TableName() string { return "exp_approval_action_detail" }
