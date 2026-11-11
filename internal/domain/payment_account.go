package domain

// PaymentAccount 是付款主体名下的外部受控账户引用，不保存银行账号原文。
type PaymentAccount struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	FinanceEntityID string `json:"finance_entity_id"`
	AccountRef      string `json:"account_ref"`
	Active          bool   `json:"active"`
}
