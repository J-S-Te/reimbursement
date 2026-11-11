package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"strings"
	"time"
)

type PolicyLifecycleInput struct {
	CommandInput
	Kind                       string `json:"kind"`
	Reason                     string `json:"reason"`
	ReplacementID              string `json:"replacement_id"`
	ReplacementExpectedVersion int64  `json:"replacement_expected_version"`
}

// lockPolicyTenant 串行化租户政策发布，避免不同草稿并发通过冲突检查。
// 参数：tx 为本地事务，tenant 为可信租户。
// 返回值：互斥行创建或锁定错误。
func lockPolicyTenant(tx *gorm.DB, tenant string) error {
	key := "policy:" + tenant
	if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.Sequence{ID: key}).Error; e != nil {
		return e
	}
	var row store.Sequence
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", key).First(&row).Error
}

// policyShape 提取白名单配置的业务适用键、日期区间和启用状态。
// 参数：kind 为配置种类，raw 为不可变配置内容。
// 返回值：适用键、起止日期、启用标记及解析错误。
func policyShape(kind, raw string) (string, string, string, bool, error) {
	var values map[string]any
	if e := json.Unmarshal([]byte(raw), &values); e != nil {
		return "", "", "", false, e
	}
	fields := map[string][]string{"historical_tail_policies": {"org_id", "project_id"}, "standards": {"org_id", "fee_type", "city_level", "grade", "priority", "transport_category", "accommodation_category"}, "route_rules": {"org_id", "route_type"}, "assignments": {"org_id", "project_id", "role", "subject_id"}, "employee_profiles": {"subject_id", "org_id"}, "invoice_rules": {"invoice_type"}, "scopes": {"subject_id"}, "finance_entities": {"tax_id"}, "payment_accounts": {"finance_entity_id", "account_ref"}, "payee_profiles": {"subject_id", "account_ref"}, "city_mappings": {"org_id", "source_city"}, "archive_retention_policies": {"org_id"}}[kind]
	if len(fields) == 0 {
		return "", "", "", false, domain.Fail("INVALID_CONFIGURATION", "不支持此类政策生命周期", 422)
	}
	key := map[string]any{}
	for _, field := range fields {
		if field == "priority" {
			value, ok := values[field].(float64)
			if !ok {
				value = 0
			}
			key[field] = value
		} else {
			value, _ := values[field].(string)
			key[field] = value
		}
	}
	encoded, e := encode(key)
	start, _ := values["valid_from"].(string)
	end, _ := values["valid_until"].(string)
	active := kind != "employee_profiles" && kind != "invoice_rules" && kind != "finance_entities" && kind != "payee_profiles" && kind != "payment_accounts"
	if kind == "historical_tail_policies" {
		enabled, _ := values["enabled"].(bool)
		active = enabled
	}
	if flag, exists := values["active"].(bool); exists {
		active = flag
	}
	return encoded, start, end, active, e
}

// periodsOverlap 判断两个闭区间业务日期是否存在交集。
// 参数：各起止日期，空边界分别表示无界。
// 返回值：区间是否重叠。
func periodsOverlap(aStart, aEnd, bStart, bEnd string) bool {
	return (aEnd == "" || bStart == "" || bStart <= aEnd) && (bEnd == "" || aStart == "" || aStart <= bEnd)
}

// checkPublicationConflict 在租户发布锁保护下阻断相同适用范围的重叠有效版本。
// 参数：tx 为事务，row 为候选版本，ignored 为本次原子替换的旧版本。
// 返回值：配置冲突或读取错误。
func checkPublicationConflict(tx *gorm.DB, row store.Config, ignored string) error {
	key, start, end, active, e := policyShape(row.Kind, row.ContentJSON)
	if e != nil || !active {
		return e
	}
	rows, e := configs(tx, row.TenantID, row.Kind, true)
	if e != nil {
		return e
	}
	for _, other := range rows {
		if other.ID == row.ID || other.ID == ignored {
			continue
		}
		otherKey, from, until, enabled, err := policyShape(other.Kind, other.ContentJSON)
		if err != nil {
			return err
		}
		if enabled && key == otherKey && periodsOverlap(start, end, from, until) {
			return domain.Fail("POLICY_EFFECTIVE_CONFLICT", "已有适用范围和有效期重叠的发布版本，请原子替换或先受控退休", 409)
		}
	}
	return nil
}

// lifecyclePolicy 只追加退休事实，并在同事务发布已审核替换草稿。
// 参数：tx 为事务，cc 为具有当前发布能力的命令，payload 为原因和双对象版本。
// 返回值：冻结生命周期回执、旧配置标识及错误。
func (s *Service) lifecyclePolicy(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input PolicyLifecycleInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return nil, "", domain.Fail("REASON_REQUIRED", "退休或替换必须说明审批原因", 422)
	}
	if e := lockPolicyTenant(tx, cc.actor.TenantID); e != nil {
		return nil, "", e
	}
	ids := []string{cc.resourceID}
	if cc.operation == "config.replace" {
		if input.ReplacementID == "" || input.ReplacementID == cc.resourceID {
			return nil, "", domain.Fail("INVALID_REPLACEMENT", "请指定另一份同类配置草稿", 422)
		}
		ids = append(ids, input.ReplacementID)
	}
	sort.Strings(ids)
	var rows []store.Config
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id IN ?", cc.actor.TenantID, ids).Order("id").Find(&rows).Error; e != nil {
		return nil, "", e
	}
	versions := map[string]store.Config{}
	for _, row := range rows {
		versions[row.ID] = row
	}
	old, exists := versions[cc.resourceID]
	if !exists || old.Kind != input.Kind {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "原配置不存在或类型不匹配", 404)
	}
	if e := verifyVersion(old.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if !old.Published {
		return nil, "", domain.Fail("POLICY_NOT_PUBLISHED", "仅已发布版本可以退休或替换", 409)
	}
	var retired int64
	if e := tx.Model(&store.PolicyLifecycle{}).Where("tenant_id = ? AND config_id = ?", cc.actor.TenantID, old.ID).Count(&retired).Error; e != nil {
		return nil, "", e
	}
	if retired > 0 {
		return nil, "", domain.Fail("POLICY_ALREADY_RETIRED", "此版本已退休，不能恢复或再次替换", 409)
	}
	var replacementID string
	if cc.operation == "config.replace" {
		next, found := versions[input.ReplacementID]
		if !found || next.Kind != old.Kind || next.Published {
			return nil, "", domain.Fail("INVALID_REPLACEMENT", "替换目标必须是同种配置的未发布草稿", 422)
		}
		if e := verifyVersion(next.Version, input.ReplacementExpectedVersion); e != nil {
			return nil, "", e
		}
		if e := validateConfig(next.Kind, next.ContentJSON); e != nil {
			return nil, "", e
		}
		_, from, until, _, e := policyShape(old.Kind, old.ContentJSON)
		if e != nil {
			return nil, "", e
		}
		_, newFrom, newUntil, active, e := policyShape(next.Kind, next.ContentJSON)
		if e != nil {
			return nil, "", e
		}
		oldKey, e := replacementScope(old.Kind, old.ContentJSON)
		if e != nil {
			return nil, "", e
		}
		newKey, e := replacementScope(next.Kind, next.ContentJSON)
		if e != nil {
			return nil, "", e
		}
		if oldKey != newKey || !active {
			return nil, "", domain.Fail("REPLACEMENT_SCOPE_MISMATCH", "替换必须保持业务适用范围且新版本处于启用状态", 422)
		}
		day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
		if (from == "" || from <= day) && (until == "" || until >= day) && ((newFrom != "" && newFrom > day) || (newUntil != "" && newUntil < day)) {
			return nil, "", domain.Fail("REPLACEMENT_EFFECTIVE_GAP", "当前有效政策不能替换成当前尚未生效或已过期版本", 422)
		}
		if e = checkPublicationConflict(tx, next, old.ID); e != nil {
			return nil, "", e
		}
		var body map[string]any
		if e = json.Unmarshal([]byte(next.ContentJSON), &body); e != nil {
			return nil, "", e
		}
		body["published"] = true
		body["version"] = next.Version + 1
		raw, e := encode(body)
		if e != nil {
			return nil, "", e
		}
		r := tx.Model(&store.Config{}).Where("tenant_id = ? AND id = ? AND version = ? AND published = ?", next.TenantID, next.ID, next.Version, false).Updates(map[string]any{"version": next.Version + 1, "published": true, "content_json": raw})
		if r.Error != nil {
			return nil, "", r.Error
		}
		if r.RowsAffected != 1 {
			return nil, "", domain.Fail("VERSION_CONFLICT", "替换草稿已改变", 409)
		}
		replacementID = next.ID
	}
	hash, e := digest(json.RawMessage(old.ContentJSON))
	if e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	at := time.Now().UTC()
	entry := store.PolicyLifecycle{ID: id, TenantID: old.TenantID, ConfigID: old.ID, Kind: old.Kind, Operation: cc.operation, ReplacementID: replacementID, Actor: cc.actor.SubjectID, Reason: input.Reason, PreviousVersion: old.Version, CurrentVersion: old.Version + 1, ContentHash: hash, EffectiveAt: at}
	if e = tx.Create(&entry).Error; e != nil {
		return nil, "", e
	}
	r := tx.Model(&store.Config{}).Where("tenant_id = ? AND id = ? AND version = ?", old.TenantID, old.ID, old.Version).Update("version", old.Version+1)
	if r.Error != nil {
		return nil, "", r.Error
	}
	if r.RowsAffected != 1 {
		return nil, "", domain.Fail("VERSION_CONFLICT", "原配置已改变", 409)
	}
	return map[string]any{"id": old.ID, "version": old.Version + 1, "lifecycle_status": "RETIRED", "retired_at": at, "replaced_by_id": replacementID, "lifecycle_event_id": id}, old.ID, nil
}

// replacementScope 忽略可更换的负责人、账户或标准优先级，保留业务归属边界。
// 参数：kind 为配置种类，raw 为已校验内容。
// 返回值：替换适用范围键或解析错误。
func replacementScope(kind, raw string) (string, error) {
	var values map[string]any
	if e := json.Unmarshal([]byte(raw), &values); e != nil {
		return "", e
	}
	switch kind {
	case "standards":
		delete(values, "priority")
	case "assignments":
		delete(values, "subject_id")
	case "payee_profiles":
		delete(values, "account_ref")
	}
	normalized, e := encode(values)
	if e != nil {
		return "", e
	}
	key, _, _, _, e := policyShape(kind, normalized)
	return key, e
}
