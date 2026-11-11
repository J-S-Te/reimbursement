package application

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type selectionDirectory interface {
	LookupPerson(context.Context, domain.Actor, string, string) (domain.PersonOption, bool, error)
	SearchPeople(context.Context, domain.Actor, string, int, int) (domain.PersonOptionsPage, error)
}

// PersonnelOptions 按具体业务用途返回已校验范围的安全姓名选项。
// 参数：ctx/a 为可信调用；purpose 为用途；resourceID 为任务；keyword/page/size 为搜索分页。
// 返回值：授权姓名分页；动作、任务、组织、目录或参数异常时拒绝。
func (s *Service) PersonnelOptions(ctx context.Context, a domain.Actor, purpose, resourceID, keyword string, page, size int) (json.RawMessage, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > 100 || len(resourceID) > 128 {
		return nil, domain.Fail("OPTIONS_INVALID", "搜索条件过长", 422)
	}
	db := s.db.WithContext(ctx)
	var items []domain.NameOption
	var err error
	switch purpose {
	case "transfer", "recovery":
		items, err = s.taskPersonnelOptions(ctx, db, a, purpose, resourceID)
	case "applicant":
		items, err = s.applicantPersonnelOptions(ctx, db, a, keyword)
	case "policy":
		items, err = s.policyPersonnelOptions(ctx, db, a, keyword)
	default:
		return nil, domain.Fail("OPTIONS_PURPOSE_INVALID", "不支持的人员选择用途", 422)
	}
	if err != nil {
		return nil, err
	}
	filtered := []domain.NameOption{}
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Name), strings.ToLower(keyword)) {
			filtered = append(filtered, item)
		}
	}
	return nameOptionsPage(filtered, page, size)
}

// nameOptionsPage 对已授权结果确定性排序并分页，不泄漏被裁剪目录总数。
// 参数：items 为已授权姓名；page/size 为请求分页。
// 返回值：安全姓名列表及授权结果总数。
func nameOptionsPage(items []domain.NameOption, page, size int) (json.RawMessage, error) {
	page, size, offset := pageArgs(map[string]string{"page": strconv.Itoa(page), "page_size": strconv.Itoa(size)})
	if size > 50 {
		size = 50
		offset = (page - 1) * size
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name == items[j].Name {
			return items[i].ID < items[j].ID
		}
		return items[i].Name < items[j].Name
	})
	end := offset + size
	if offset > len(items) {
		offset = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	return json.Marshal(map[string]any{"items": items[offset:end], "total": len(items), "page": page, "page_size": size})
}

// lookupPersonnelIDs 仅查询业务层已经限定的精确候选，拒绝无界目录扫描。
// 参数：ctx/a 为调用身份；ids 为候选集合；permission 为所需当前能力。
// 返回值：有当前任职资格的安全姓名；目录异常失败关闭。
func (s *Service) lookupPersonnelIDs(ctx context.Context, a domain.Actor, ids map[string]bool, permission string) ([]domain.NameOption, error) {
	directory, ok := s.deps.Personnel.(selectionDirectory)
	if !ok {
		return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员姓名服务未配置", 503)
	}
	if len(ids) > 100 {
		return nil, domain.Fail("OPTIONS_LIMIT", "候选人员超过受控上限，请缩小业务范围", 422)
	}
	result := []domain.NameOption{}
	for _, id := range mapKeys(ids) {
		person, found, err := directory.LookupPerson(ctx, a, id, permission)
		if err != nil {
			return nil, err
		}
		if found && person.ID == id && person.Name != "" {
			result = append(result, domain.NameOption{ID: id, Name: person.Name})
		}
	}
	return result, nil
}

// applicantPersonnelOptions 只为当前可读取单据中的真实申请人解析姓名。
// 参数：ctx/db/a 为可信查询边界；keyword 为姓名筛选。
// 返回值：可见申请人姓名；无读取能力或目录故障拒绝。
func (s *Service) applicantPersonnelOptions(ctx context.Context, db *gorm.DB, a domain.Actor, keyword string) ([]domain.NameOption, error) {
	if err := require(a, "expense.claim.read"); err != nil {
		return nil, err
	}
	claims, err := readableClaims(db, a, true)
	if err != nil {
		return nil, err
	}
	if keyword != "" {
		return s.searchPersonnelOptions(ctx, a, keyword, func(people []domain.PersonOption) (map[string]bool, error) {
			ids := []string{}
			for _, person := range people {
				ids = append(ids, person.ID)
			}
			var visible []string
			err := claims.Session(&gorm.Session{}).Where("applicant IN ?", ids).
				Distinct("applicant").Pluck("applicant", &visible).Error
			allowed := map[string]bool{}
			for _, id := range visible {
				allowed[id] = true
			}
			return allowed, err
		})
	}
	var subjects []string
	if err = claims.Distinct("applicant").Limit(101).Pluck("applicant", &subjects).Error; err != nil {
		return nil, err
	}
	if len(subjects) > 100 {
		return nil, domain.Fail("OPTIONS_SEARCH_REQUIRED", "申请人较多，请输入姓名搜索", 422)
	}
	ids := map[string]bool{}
	for _, id := range subjects {
		if id != "" {
			ids[id] = true
		}
	}
	return s.lookupPersonnelIDs(ctx, a, ids, "")
}

// policyPersonnelOptions 允许政策首配读取明确授权组织内的当前真实任职人员。
// 参数：ctx/db/a 为可信配置管理边界；keyword 为姓名搜索。
// 返回值：组织内姓名；管理权限不能替代独立范围授权。
func (s *Service) policyPersonnelOptions(ctx context.Context, db *gorm.DB, a domain.Actor, keyword string) ([]domain.NameOption, error) {
	if err := require(a, "expense.config.manage"); err != nil {
		return nil, err
	}
	orgs, all, err := scopeOrganizations(db, a)
	if err != nil {
		return nil, err
	}
	if !all && len(orgs) == 0 {
		return nil, domain.Fail("FORBIDDEN", "尚未配置人员选项的明确组织范围", 403)
	}
	if keyword == "" {
		return []domain.NameOption{}, nil
	}
	allowed := map[string]bool{}
	for _, org := range orgs {
		allowed[org] = true
	}
	return s.searchPersonnelOptions(ctx, a, keyword, func(people []domain.PersonOption) (map[string]bool, error) {
		ids := map[string]bool{}
		for _, person := range people {
			for _, org := range person.Organizations {
				if all || allowed[org.ID] {
					ids[person.ID] = true
				}
			}
		}
		return ids, nil
	})
}

// searchPersonnelOptions 有界搜索后逐个应用业务范围，返回总数不包含无权用户。
// 参数：ctx/a 为身份；keyword 为姓名；allowed 为当前业务范围判断。
// 返回值：授权姓名；目录规模过大要求缩小搜索，任何依赖故障失败关闭。
func (s *Service) searchPersonnelOptions(ctx context.Context, a domain.Actor, keyword string, allowed func([]domain.PersonOption) (map[string]bool, error)) ([]domain.NameOption, error) {
	directory, ok := s.deps.Personnel.(selectionDirectory)
	if !ok {
		return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员姓名服务未配置", 503)
	}
	result := []domain.NameOption{}
	seen := map[string]bool{}
	for page := 1; page <= 20; page++ {
		response, err := directory.SearchPeople(ctx, a, keyword, page, 50)
		if err != nil {
			return nil, err
		}
		if response.Total > 1000 {
			return nil, domain.Fail("OPTIONS_SEARCH_REQUIRED", "姓名匹配过多，请输入更完整姓名", 422)
		}
		authorized, err := allowed(response.Items)
		if err != nil {
			return nil, err
		}
		for _, person := range response.Items {
			if person.ID == "" || person.Name == "" || seen[person.ID] {
				return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员目录响应不一致", 503)
			}
			seen[person.ID] = true
			if authorized[person.ID] {
				result = append(result, domain.NameOption{ID: person.ID, Name: person.Name})
			}
		}
		if int64(page*50) >= response.Total {
			return result, nil
		}
		if len(response.Items) == 0 {
			return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员目录分页不完整", 503)
		}
	}
	return nil, domain.Fail("OPTIONS_SEARCH_REQUIRED", "姓名匹配过多，请缩小搜索", 422)
}

// DocumentOptions 返回本人已就绪的指定用途文件，不泄漏票据或完整元数据。
// 参数：ctx/a 为可信身份；purpose 为文件用途；keyword/page/size 为文件名与分页。
// 返回值：文件名、状态及提交引用；无权、非法用途或超长搜索拒绝。
func (s *Service) DocumentOptions(ctx context.Context, a domain.Actor, purpose, keyword string, page, size int) (json.RawMessage, error) {
	var err error
	switch purpose {
	case "reimbursement.invoice.original":
		err = require(a, "expense.invoice.upload")
	case "reimbursement.claim.evidence":
		err = authorizeClaimEvidenceUpload(a)
	case "reimbursement.payment.voucher":
		err = authorizeVoucherUpload(a)
	default:
		return nil, domain.Fail("FILE_PURPOSE_INVALID", "不支持的文件选择用途", 422)
	}
	if err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > 100 {
		return nil, domain.Fail("OPTIONS_INVALID", "文件名搜索过长", 422)
	}
	db := s.db.WithContext(ctx)
	name := queryJSONText(db, "content_json", "original_name")
	q := db.Model(&store.Document{}).Where("tenant_id = ? AND owner = ? AND file_id <> ? AND status IN ?",
		a.TenantID, a.SubjectID, "", []string{"READY", "PARSED", "REVIEW_REQUIRED"}).
		Where(queryJSONText(db, "content_json", "purpose")+" = ?", purpose).
		Where(name+" IS NOT NULL AND "+name+" <> ?", "")
	if keyword != "" {
		q = q.Where(name+" LIKE ? ESCAPE '!'", queryLikePattern(keyword))
	}
	var total int64
	if err = q.Count(&total).Error; err != nil {
		return nil, err
	}
	page, size, offset := pageArgs(map[string]string{"page": strconv.Itoa(page), "page_size": strconv.Itoa(size)})
	if size > 50 {
		size = 50
		offset = (page - 1) * size
	}
	var rows []store.Document
	if err = q.Order("created_at DESC,id DESC").Limit(size).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, row := range rows {
		var metadata struct {
			Name string `json:"original_name"`
		}
		if err = json.Unmarshal([]byte(row.ContentJSON), &metadata); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": row.ID, "file_id": row.FileID, "name": metadata.Name, "status": row.Status})
	}
	return json.Marshal(map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}
