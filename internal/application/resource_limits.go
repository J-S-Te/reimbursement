package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
)

// commandResourceBounds 在文件、项目或人员网络预检前拒绝超限凭证集合。
// 参数：operation 为固定服务端动作，payload 为命令内容。
// 返回值：参数格式错误或资源超限错误。
func commandResourceBounds(operation string, payload json.RawMessage) error {
	var ids []string
	switch operation {
	case "invoice.create", "invoice.confirm":
		var input domain.Invoice
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.FileIDs
	case "invoice.verify":
		var input VerificationInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.EvidenceFileIDs
	case "payment.exception.record":
		var input PaymentExceptionInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.VoucherFileIDs
	case "payment.exception.review":
		var input PaymentExceptionReviewInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.EvidenceFileIDs
	case "payment.record":
		var input PaymentInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.VoucherFileIDs
	case "reversal.request":
		var input ReversalInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		ids = input.EvidenceFileIDs
	default:
		return nil
	}
	if len(ids) == 0 || len(ids) > 20 {
		return domain.Fail("FILE_REFERENCE_LIMIT", "每次操作必须关联1至20个文件引用，不能截断附件", 422)
	}
	return nil
}
