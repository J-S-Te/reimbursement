package application

import (
	"errors"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// acquirePaymentBinding 通过独立唯一约束占用当前银行流水或现金凭证号。
// 参数：tx 为持有单据锁的事务，payment 为本次新建的不可变付款事实。
// 返回值：已占用返回业务冲突，其余数据库错误原样返回并回滚付款。
func acquirePaymentBinding(tx *gorm.DB, payment store.Payment) error {
	var count int64
	err := tx.Model(&store.PaymentBinding{}).Where(
		"tenant_id = ? AND method = ? AND source_account_ref = ? AND transaction_no = ?",
		payment.TenantID, payment.Method, payment.SourceAccountRef, payment.TransactionNo,
	).Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return paymentBindingConflict()
	}
	err = tx.Create(&store.PaymentBinding{ID: payment.ID, TenantID: payment.TenantID,
		Method: payment.Method, SourceAccountRef: payment.SourceAccountRef,
		TransactionNo: payment.TransactionNo, PaymentID: payment.ID, ClaimID: payment.ClaimID,
	}).Error
	var duplicate *driver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || errors.As(err, &duplicate) && duplicate.Number == 1062 {
		return paymentBindingConflict()
	}
	return err
}

// paymentBindingConflict 生成不泄漏原占用单据的流水防重错误。
// 参数：无。
// 返回值：当前流水已经登记的业务冲突。
func paymentBindingConflict() error {
	return domain.Fail("PAYMENT_TRANSACTION_CONFLICT", "此渠道、账户与流水号已有登记，请核对原付款", 409)
}

// releaseNoPaymentBinding 仅释放独立批准实际未付所对应的原付款占用。
// 参数：tx 为冲正事务，payment 为原付款，data 为已独立批准的冲正事实。
// 返回值：退款不释放；原付款绑定失配时失败，绝不删除后来的新占用。
func releaseNoPaymentBinding(tx *gorm.DB, payment store.Payment, data ReversalData) error {
	if data.FactKind != "NO_PAYMENT" {
		return nil
	}
	if data.Status != "APPROVED" || data.PaymentID != payment.ID || data.ClaimID != payment.ClaimID ||
		data.Approver == "" || data.Approver == data.Requester || data.Approver == payment.Actor {
		return domain.Fail("PAYMENT_BINDING_CONFLICT", "实际未付的独立批准证据不完整", 409)
	}
	result := tx.Where("tenant_id = ? AND payment_id = ? AND claim_id = ? AND method = ? AND source_account_ref = ? AND transaction_no = ?",
		payment.TenantID, payment.ID, payment.ClaimID, payment.Method, payment.SourceAccountRef, payment.TransactionNo,
	).Delete(&store.PaymentBinding{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return domain.Fail("PAYMENT_BINDING_CONFLICT", "原付款流水占用已变化，请核实后重试", 409)
	}
	return nil
}
