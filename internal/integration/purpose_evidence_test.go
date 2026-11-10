package integration

import "testing"

// TestClaimEvidencePurposeKeepsInvoiceBoundary 验证各上传用途的格式边界：
// 发票原件允许电子票与拍照件，申请证据允许 PDF/图片但不得混入 OFD，未知用途一律拒绝。
// 参数：t为测试上下文。
// 返回值：无。
func TestClaimEvidencePurposeKeepsInvoiceBoundary(t *testing.T) {
	for _, media := range []string{"application/pdf", "application/ofd", "application/vnd.ofd", "image/png", "image/jpeg"} {
		if !filePurpose(Upload{Purpose: "reimbursement.invoice.original", MediaType: media}) {
			t.Fatalf("合法发票原件格式被拒绝: %s", media)
		}
	}
	for _, media := range []string{"application/pdf", "image/png", "image/jpeg"} {
		if !filePurpose(Upload{Purpose: "reimbursement.claim.evidence", MediaType: media}) {
			t.Fatalf("合法申请证据被拒绝: %s", media)
		}
	}
	for _, upload := range []Upload{
		{Purpose: "reimbursement.claim.evidence", MediaType: "application/ofd"},
		{Purpose: "reimbursement.claim.evidence", MediaType: "text/html"},
		{Purpose: "reimbursement.invoice.original", MediaType: "text/html"},
		{Purpose: "reimbursement.invoice.original", MediaType: "application/zip"},
		{Purpose: "reimbursement.unknown", MediaType: "application/pdf"},
	} {
		if filePurpose(upload) {
			t.Fatalf("非法用途或格式被放行: %+v", upload)
		}
	}
}
