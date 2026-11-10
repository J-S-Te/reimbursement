package application

import (
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestMappedAuthoritativeDestinationProducesCityMatch 验证明确城市映射能消除城市未知但不生成旅行审批。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestMappedAuthoritativeDestinationProducesCityMatch(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	facts := travelFixtureSnapshot("MATCH")
	facts.Destination = "上海市"
	facts.Transport = "TRAIN"
	facts.AccommodationNeed = "NO"
	refreshTravelChangeDigest(t, facts)
	enableTravelFixture(t, s, id, nil, facts)
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].CityCode = "310100"
	raw, _ := json.Marshal(c)
	if err := db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	seedPolicy(t, db, "city_mappings", domain.CityMapping{ID: "shanghai", Version: 2, OrgID: "org", SourceCity: "上海市", CityCode: "310100", CityLevel: "A", ValidFrom: "2026-01-01", Active: true, Published: true})
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "mapped-city-submit", "expected_version": 1})
	c, _, _ = loadClaim(db, "t", id, false)
	assessment := c.Items[0].TravelAssessment
	if assessment == nil || assessment.Status != "MATCH" || assessment.References[0].CityMapping == nil || assessment.References[0].CityMapping.PolicyID != "shanghai" {
		t.Fatalf("城市映射未固定版本或仍错误未知: %+v", assessment)
	}
	if facts.CityCodeStatus != "UNKNOWN" {
		t.Fatal("本地映射改写了权威旅行事实")
	}
}
