package application

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// policyCode 检查分类代码的有界格式，不猜测管理者定义的业务分类。
// 参数：value 为分类标识，emptyAllowed 表示兼容旧政策的空维度。
// 返回值：非空白且无控制符的最多64字节代码是否合法。
func policyCode(value string, emptyAllowed bool) bool {
	if value == "" {
		return emptyAllowed
	}
	if len(value) > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// structuredCityCode 校验明确六位行政代码，不从城市自由文本推导代码。
// 参数：code 为申请人或政策提供的结构化代码。
// 返回值：是否为非全零六位十进制代码；行政真实性由受控映射来源负责。
func structuredCityCode(code string) bool {
	if len(code) != 6 || code == "000000" {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validateStandardDimensions 校验分类维度及无票证明类型策略的一致性。
// 参数：standard 为待保存的费用标准。
// 返回值：不合法分类、证明白名单或互斥选项的业务错误。
func validateStandardDimensions(standard domain.Standard) error {
	invalid := domain.Fail("INVALID_CONFIGURATION", "标准分类或无票证明政策不合法", 422)
	if !policyCode(standard.TransportCategory, true) || !policyCode(standard.AccommodationCategory, true) {
		return invalid
	}
	if standard.RequireNoInvoiceProof && (!standard.AllowNoInvoice || len(standard.NoInvoiceProofTypes) == 0) {
		return invalid
	}
	seen := map[string]bool{}
	for _, kind := range standard.NoInvoiceProofTypes {
		if !validNoInvoiceProofType(kind) || seen[kind] {
			return invalid
		}
		seen[kind] = true
	}
	return nil
}

// validNoInvoiceProofType 限定无票证明声明类型，附件仍独立核验归属和用途。
// 参数：kind 为申请人选择的证明类型。
// 返回值：是否为受支持的付款、服务确认或其他说明证明。
func validNoInvoiceProofType(kind string) bool {
	return kind == "PAYMENT_PROOF" || kind == "SERVICE_CONFIRMATION" || kind == "OTHER"
}

// standardDimensionsMatch 精确匹配交通与住宿两个独立维度，不把空分类当通配。
// 参数：standard 为候选政策，item 为当前费用。
// 返回值：两个分类是否同时精确相等。
func standardDimensionsMatch(standard domain.Standard, item domain.Item) bool {
	return standard.TransportCategory == item.TransportCategory &&
		standard.AccommodationCategory == item.AccommodationCategory
}

// applyItemProofPolicy 冻结兼容标记并执行独立的无票证明政策。
// 参数：item 为待提交费用，standard 为唯一匹配的不可变政策，index 为费用位置。
// 返回值：缺少类型或证据时返回可定位的费用错误。
func applyItemProofPolicy(item *domain.Item, standard domain.Standard, index int) error {
	item.PolicyCompatibility = nil
	if standard.TransportCategory == "" && standard.AccommodationCategory == "" {
		item.PolicyCompatibility = append(item.PolicyCompatibility, "LEGACY_STANDARD_DIMENSIONS")
	}
	if len(item.InvoiceUsages) > 0 {
		return nil
	}
	if !standard.RequireNoInvoiceProof {
		item.PolicyCompatibility = append(item.PolicyCompatibility, "LEGACY_NO_INVOICE_PROOF_POLICY")
		return nil
	}
	accepted := false
	for _, kind := range standard.NoInvoiceProofTypes {
		accepted = accepted || kind == item.NoInvoiceProofType
	}
	if !accepted || !validNoInvoiceProofType(item.NoInvoiceProofType) {
		return itemPolicyFailure(index, "no_invoice_proof_type", "NO_INVOICE_PROOF_REQUIRED", "请选择该标准允许的无票证明类型")
	}
	if len(item.NoInvoiceEvidenceFileIDs) == 0 || len(item.NoInvoiceEvidenceFileIDs) > 20 {
		return itemPolicyFailure(index, "no_invoice_evidence_file_ids", "NO_INVOICE_PROOF_REQUIRED", "请提供独立无票证明附件")
	}
	seen := map[string]bool{}
	for _, id := range item.NoInvoiceEvidenceFileIDs {
		if strings.TrimSpace(id) == "" || len(id) > 128 || seen[id] {
			return itemPolicyFailure(index, "no_invoice_evidence_file_ids", "NO_INVOICE_PROOF_REQUIRED", "无票证明附件标识为空、重复或超长")
		}
		seen[id] = true
	}
	return nil
}

// itemPolicyFailure 创建指向具体费用字段的中文业务错误。
// 参数：index 为费用序号，field 为属性，code 为错误码，message 为用户说明。
// 返回值：含结构化字段定位的费用校验错误。
func itemPolicyFailure(index int, field, code, message string) error {
	problem := domain.Fail(code, message, 422).(*domain.Problem)
	problem.FieldErrors = []domain.FieldError{{Field: fmt.Sprintf("items[%d].%s", index, field), Message: message}}
	return problem
}

// validateAdditionalPolicy 校验城市映射与组织档案保留政策的明确边界。
// 参数：kind 为新配置种类，raw 为标准化配置JSON。
// 返回值：格式、有效期、代码或起算规则不合法的错误。
func validateAdditionalPolicy(kind, raw string) error {
	invalid := domain.Fail("INVALID_CONFIGURATION", "城市映射或档案保留政策字段不合法", 422)
	switch kind {
	case "city_mappings":
		var value domain.CityMapping
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return err
		}
		if value.OrgID == "" || strings.TrimSpace(value.SourceCity) != value.SourceCity || value.SourceCity == "" ||
			len(value.SourceCity) > 128 || !structuredCityCode(value.CityCode) || !policyCode(value.CityLevel, false) ||
			!validPeriod(value.ValidFrom, value.ValidUntil) {
			return invalid
		}
	case "archive_retention_policies":
		var value domain.ArchiveRetentionPolicy
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return err
		}
		if value.OrgID == "" || value.RetentionMonths < 1 || value.RetentionMonths > 1200 ||
			value.StartBasis != "SUBMISSION_DATE" || strings.TrimSpace(value.Basis) == "" || len(value.Basis) > 2000 ||
			!validPeriod(value.ValidFrom, value.ValidUntil) {
			return invalid
		}
	}
	return nil
}

// validateItemDimensions 校验费用分类和结构化城市代码的输入边界。
// 参数：item 为费用输入，index 为费用位置。
// 返回值：分类或代码不合法时返回精确字段错误。
func validateItemDimensions(item domain.Item, index int) error {
	if !policyCode(item.TransportCategory, true) {
		return itemPolicyFailure(index, "transport_category", "INVALID_CATEGORY", "交通分类代码不合法")
	}
	if !policyCode(item.AccommodationCategory, true) {
		return itemPolicyFailure(index, "accommodation_category", "INVALID_CATEGORY", "住宿分类代码不合法")
	}
	if item.CityCode != "" && !structuredCityCode(item.CityCode) {
		return itemPolicyFailure(index, "city_code", "INVALID_CITY_CODE", "请填写六位结构化城市代码")
	}
	return nil
}
