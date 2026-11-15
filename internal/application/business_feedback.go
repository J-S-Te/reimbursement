package application

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// invoiceUsageFailure 用途：为已通过所属人核验的票据拒绝补充真实费用行坐标。
// 参数：claim 为本人的费用单据，invoiceID 为已授权票据，err 为原有业务拒绝。
// 返回值：保留错误码和状态的字段错误；非业务异常保持原样。
func invoiceUsageFailure(claim domain.Claim, invoiceID string, err error) error {
	var original *domain.Problem
	if !errors.As(err, &original) {
		return err
	}
	problem := *original
	problem.FieldErrors = nil
	for index, item := range claim.Items {
		for usageIndex, usage := range item.InvoiceUsages {
			if usage.InvoiceID != invoiceID {
				continue
			}
			problem.FieldErrors = append(problem.FieldErrors, domain.FieldError{
				Field:   fmt.Sprintf("items[%d].invoice_usages", index),
				Message: fmt.Sprintf("第%d项费用的第%d张发票：%s", index+1, usageIndex+1, problem.Message),
			})
		}
	}
	return &problem
}

// invoiceUseStateFailure 用途：按既有判定分别说明已授权票据为何不能用于本单。
// 参数：row 为票据状态，invoice 为可信票面，used 为本单累计使用金额。
// 返回值：沿用原错误码的明确业务原因；符合既有条件时为空。
func invoiceUseStateFailure(row store.Invoice, invoice domain.Invoice, used domain.Money) error {
	message := ""
	switch {
	case invoice.ReviewStatus != "CONFIRMED":
		message = "发票尚未确认，请先对照原件完成人工复核后再提交"
	case invoice.VerificationStatus == "MISMATCH":
		message = "发票核验结果存在异常，请由财务核实后处理，暂不能用于本单"
	case row.IdentityKey == nil:
		message = "发票业务身份尚未完整确认，请先对照原件完成人工复核"
	case used > invoice.Total:
		message = "本单累计使用额超过票面金额，请核对各费用项的发票使用额"
	}
	if message == "" {
		return nil
	}
	return domain.Fail("INVOICE_NOT_AVAILABLE", message, 422)
}

// paymentFieldFailure 用途：构造与实际付款表单一致的单字段校验错误。
// 参数：code 为既有错误码，field 为真实输入字段，message 为安全业务指引。
// 返回值：422 业务错误，不包含请求值或内部账户详情。
func paymentFieldFailure(code, field, message string) error {
	return &domain.Problem{Code: code, Message: message, Status: 422,
		FieldErrors: []domain.FieldError{{Field: field, Message: message}}}
}

// validatePaymentFields 用途：细分现有付款校验，自动批准字段不要求用户手工填写。
// 参数：input 为本次付款请求，claim 为已授权且锁定的批准单据。
// 返回值：原有规则的具体字段拒绝；符合原规则时为空。
func validatePaymentFields(input PaymentInput, claim domain.Claim) error {
	if input.Amount <= 0 || input.Amount != claim.Payable || input.Currency != claim.Currency ||
		input.PayeeProfileID != claim.PayeeProfileID {
		return domain.Fail("PAYMENT_MISMATCH",
			"付款金额、币种或收款资料与批准快照不一致，请先刷新单据核对批准结果；"+
				"仍不一致时联系财务管理员", 422)
	}
	if _, err := time.Parse("2006-01-02", input.PaidDate); err != nil {
		return paymentFieldFailure("INVALID_DATE", "paid_date", "实际付款日期无效，请重新选择真实付款日期")
	}
	if input.Method != "BANK" && input.Method != "CASH" {
		return paymentFieldFailure("INVALID_PAYMENT_METHOD", "method", "请选择银行转账或现金作为实际付款方式")
	}
	if input.SourceAccountRef == "" || len(input.SourceAccountRef) > 128 {
		return paymentFieldFailure("PAYMENT_EVIDENCE_REQUIRED", "source_account_ref",
			"请选择有效的受控付款账户；已保存的账户无效时请刷新账户选项")
	}
	if strings.TrimSpace(input.TransactionNo) == "" {
		return paymentFieldFailure("PAYMENT_EVIDENCE_REQUIRED", "transaction_no", "请填写真实银行流水号或现金凭证号")
	}
	if len(input.TransactionNo) > 128 {
		return paymentFieldFailure("PAYMENT_EVIDENCE_REQUIRED", "transaction_no",
			"银行流水号或现金凭证号超过128字节，请核对原始凭证后填写")
	}
	if len(input.VoucherFileIDs) == 0 || len(input.VoucherFileIDs) > 20 {
		return paymentFieldFailure("PAYMENT_EVIDENCE_REQUIRED", "voucher_file_ids", "付款凭证请选择1至20份已上传附件，请核对后再登记")
	}
	return nil
}
