package application

import (
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// validateExpenseNature 保留历史实际费用零值，并拒绝借款等未支持的业务语义。
// 参数：nature 为费用性质，index 为费用在申请中的位置。
// 返回值：非法费用性质的字段校验错误，允许值时为空。
func validateExpenseNature(nature string, index int) error {
	switch nature {
	case "", "ACTUAL", "PREPAYMENT":
		return nil
	default:
		return itemPolicyFailure(index, "expense_nature", "INVALID_EXPENSE_NATURE", "费用性质仅支持实际费用或预付费用")
	}
}

// applySpecialExpensePolicy 重算预付费用最小政策及冻结的财务重点复核提示。
// 参数：item 为当前费用副本，standard 为唯一匹配标准，index 为费用位置。
// 返回值：政策不允许、缺少说明或独立证明的字段错误，不产生借款或抵扣账本。
func applySpecialExpensePolicy(item *domain.Item, standard domain.Standard, index int) error {
	item.FinanceReviewRequired = false
	item.ReviewHints = nil
	if err := validateExpenseNature(item.ExpenseNature, index); err != nil {
		return err
	}
	if item.ExpenseNature != "PREPAYMENT" {
		return nil
	}
	if !standard.AllowPrepayment {
		return itemPolicyFailure(index, "expense_nature", "PREPAYMENT_NOT_ALLOWED", "当前费用标准未允许预付费用")
	}
	if strings.TrimSpace(item.Reason) == "" || utf8.RuneCountInString(item.Reason) > 2000 {
		return itemPolicyFailure(index, "reason", "PREPAYMENT_REASON_REQUIRED", "预付费用须填写最多2000字的用途与预付说明")
	}
	if err := validatePrepaymentEvidence(item.NoInvoiceEvidenceFileIDs, index); err != nil {
		return err
	}
	item.FinanceReviewRequired = true
	item.ReviewHints = []string{"预付费用需财务重点复核用途、合同及支付条件；本申请不生成借款或核销账本"}
	return nil
}

// validatePrepaymentEvidence 校验独立证明引用的形状，真实归属与用途仍由文件网关核验。
// 参数：ids 为费用证明附件集合，index 为费用位置。
// 返回值：空集合、超限、空引用或重复引用的字段错误。
func validatePrepaymentEvidence(ids []string, index int) error {
	invalid := itemPolicyFailure(index, "no_invoice_evidence_file_ids", "PREPAYMENT_EVIDENCE_REQUIRED", "预付费用须提供一至二十份独立费用证明")
	if len(ids) == 0 || len(ids) > 20 {
		return invalid
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 128 || seen[id] {
			return invalid
		}
		seen[id] = true
	}
	return nil
}
