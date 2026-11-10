package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

// mappedTravelAssessment 在不改写权威事实摘要的前提下应用已发布人工城市映射。
// 参数：tx 为配置读取边界，c 为组织单据，item 为费用，ref 为原始权威引用。
// 返回值：带政策版本的派生一致性判断；无映射仍保留未知。
func mappedTravelAssessment(tx *gorm.DB, c domain.Claim, item domain.Item, ref domain.ProjectReference) (domain.TravelAssessmentReference, error) {
	base, err := assessTravelReference(ref)
	if err != nil || ref.TravelSnapshot == nil {
		return base, err
	}
	mapping, err := findCityMapping(tx, c, item, ref.TravelSnapshot.Destination)
	if err != nil || mapping == nil {
		return base, err
	}
	facts := *ref.TravelSnapshot
	facts.CityCodeStatus = mapping.Status
	facts.ContentDigest = ""
	raw, err := json.Marshal(facts)
	if err != nil {
		return base, err
	}
	sum := sha256.Sum256(raw)
	facts.ContentDigest = hex.EncodeToString(sum[:])
	derivedRef := ref
	derivedRef.TravelSnapshot = &facts
	derived, err := assessTravelReference(derivedRef)
	derived.FactDigest = base.FactDigest
	derived.CityMapping = mapping
	return derived, err
}

// findCityMapping 根据组织、费用日和权威原文读取唯一明确城市政策。
// 参数：tx 为数据库，c 为单据，item 为费用城市代码，source 为权威目的地原文。
// 返回值：冻结政策及MATCH/MISMATCH；未配置返回nil。
func findCityMapping(tx *gorm.DB, c domain.Claim, item domain.Item, source string) (*domain.CityMappingApplication, error) {
	rows, err := configs(tx, c.TenantID, "city_mappings", true)
	if err != nil {
		return nil, err
	}
	var found *domain.CityMappingApplication
	for _, row := range rows {
		var policy domain.CityMapping
		if err = json.Unmarshal([]byte(row.ContentJSON), &policy); err != nil {
			return nil, err
		}
		if !policy.Active || policy.OrgID != c.OrgID || policy.SourceCity != source || item.OccurredDate < policy.ValidFrom || policy.ValidUntil != "" && item.OccurredDate > policy.ValidUntil {
			continue
		}
		if found != nil {
			return nil, domain.Fail("CITY_MAPPING_CONFLICT", "当前城市映射重叠，请先修复配置", 422)
		}
		status := "UNKNOWN"
		if item.CityCode != "" {
			status = "MATCH"
			if item.CityCode != policy.CityCode || item.CityLevel != "" && item.CityLevel != policy.CityLevel {
				status = "MISMATCH"
			}
		}
		version := policy.Version
		if version < 1 {
			version = row.Version
		}
		found = &domain.CityMappingApplication{PolicyID: row.ID, PolicyVersion: version, SourceCity: source, CityCode: policy.CityCode, CityLevel: policy.CityLevel, Status: status}
	}
	return found, nil
}
