package application

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// sourceLinkFixture 建立隔离来源写入夹具：本人票据、已上传原件及两次不可变解析候选。
// 参数：t 为测试上下文。
// 返回值：临时库、本人身份与已确认票据。
func sourceLinkFixture(t *testing.T) (*gorm.DB, domain.Actor, domain.Invoice) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sources.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Document{}, &store.Invoice{}, &store.InvoiceParseRun{}, &store.InvoiceSource{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	sourceSeedDocument(t, db, "t", "app", "doc", "file", "")
	run := store.InvoiceParseRun{ID: "run", TenantID: "t", DocumentID: "doc", JobID: "job", Attempt: 1, ParserVersion: "parser-v1", RulesVersion: "expense-v1", InputSHA256: "input", Status: "SUCCEEDED", ResultHash: "result", ResultJSON: `{"warnings":["w"],"documents":[{"sources":[{"page":1,"x":1,"y":2,"width":3,"text":"甲"}],"issues":["i0"]},{"sources":[{"page":2,"xml_path":"/ofd/a"}],"issues":[]}]}`}
	if err = db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	inv := domain.Invoice{ID: "invoice", TenantID: "t", Owner: "app", Version: 1, InvoiceType: "VAT", InvoiceNumber: "123", InvoiceDate: "2026-11-10", Total: 100, ReviewStatus: "CONFIRMED", FileIDs: []string{"file"}}
	raw, _ := json.Marshal(inv)
	if err = db.Create(&store.Invoice{ID: inv.ID, TenantID: "t", Owner: "app", Version: 1, ReservationState: "AVAILABLE", ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	return db, domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.review", "expense.invoice.read"}}, inv
}

// sourceSeedDocument 写入隔离原件；extra 用于附加发票候选关联等受控元数据。
// 参数：t 为测试上下文，db 为临时库，tenant/owner/fileID/id 为归属，extra 为附加JSON字段。
// 返回值：无。
func sourceSeedDocument(t *testing.T, db *gorm.DB, tenant, owner, id, fileID, extra string) {
	t.Helper()
	content := `{"purpose":"reimbursement.invoice.original","media_type":"application/pdf","filename":"` + fileID + `.pdf"` + extra + `}`
	if err := db.Create(&store.Document{ID: id, TenantID: tenant, Owner: owner, FileID: fileID, Status: "READY", Version: 1, ContentJSON: content}).Error; err != nil {
		t.Fatal(err)
	}
}

// sourceRows 读取票据当前来源行，保持数据库事实断言。
// 参数：t 为测试上下文，db 为临时库，invoiceID 为票据。
// 返回值：来源行。
func sourceRows(t *testing.T, db *gorm.DB, invoiceID string) []store.InvoiceSource {
	t.Helper()
	var rows []store.InvoiceSource
	if err := db.Where("invoice_id = ?", invoiceID).Order("candidate_index,id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// sourceViewMap 经JSON还原读取侧视图，验证写侧摘要能被读侧接受。
// 参数：t 为测试上下文，db 为临时库，inv 为票据，row 为来源行。
// 返回值：读取侧白名单视图字段。
func sourceViewMap(t *testing.T, db *gorm.DB, inv domain.Invoice, row store.InvoiceSource) map[string]any {
	t.Helper()
	view, err := sourceView(db, inv, row)
	if err != nil {
		t.Fatalf("读侧拒绝写侧来源行：%v", err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sourceErrorCode 断言受控拒绝的稳定错误码与HTTP语义。
// 参数：t 为测试上下文，err 为业务错误，code 为期望错误码，status 为期望状态码。
// 返回值：无。
func sourceErrorCode(t *testing.T, err error, code string, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %s 拒绝，实际成功", code)
	}
	problem, ok := err.(*domain.Problem)
	if !ok {
		t.Fatalf("期望领域错误 %s，实际 %T %v", code, err, err)
	}
	if problem.Code != code || problem.Status != status {
		t.Fatalf("期望 %s/%d，实际 %s/%d(%s)", code, status, problem.Code, problem.Status, problem.Message)
	}
}

// TestParsedInvoiceSourceUsesStoredRunCoordinates 验证解析来源只取同租户同原件不可变运行中的候选坐标。
// 参数：t 为测试上下文。
// 返回值：无。
func TestParsedInvoiceSourceUsesStoredRunCoordinates(t *testing.T) {
	db, actor, inv := sourceLinkFixture(t)
	index := 1
	if err := linkSelectedInvoiceSources(db, actor, inv, []InvoiceSourceSelection{{FileID: "file", DocumentID: "doc", RunID: "run", CandidateIndex: &index}}, "按解析候选补充OFD页区"); err != nil {
		t.Fatal(err)
	}
	rows := sourceRows(t, db, inv.ID)
	if len(rows) != 1 {
		t.Fatalf("期望一行解析来源，实际 %+v", rows)
	}
	if rows[0].ParseRunID != "run" || rows[0].CandidateIndex != 1 || rows[0].DocumentID != "doc" {
		t.Fatalf("解析来源关系不正确：%+v", rows[0])
	}
	if rows[0].SourcesJSON != `[{"page":2,"xml_path":"/ofd/a"}]` {
		t.Fatalf("解析来源未使用运行内候选坐标：%s", rows[0].SourcesJSON)
	}
	if rows[0].SourcesHash != parseSourceHash(rows[0].SourcesJSON) {
		t.Fatal("来源摘要与读侧规则不一致")
	}
	view := sourceViewMap(t, db, inv, rows[0])
	if view["source_kind"] != "PARSED" || view["run_id"] != "run" || view["candidate_index"] != float64(1) {
		t.Fatalf("读侧未还原解析来源：%v", view)
	}
	if view["sources_hash"] != rows[0].SourcesHash {
		t.Fatalf("读侧摘要与写侧不一致：%v", view["sources_hash"])
	}
	sources, _ := view["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["xml_path"] != "/ofd/a" {
		t.Fatalf("读侧坐标未还原：%v", view["sources"])
	}
}

// TestManualInvoiceSourceIsExplicitAndIdempotent 验证无运行选择只写显式人工关联且重复选择不产生重复行。
// 参数：t 为测试上下文。
// 返回值：无。
func TestManualInvoiceSourceIsExplicitAndIdempotent(t *testing.T) {
	db, actor, inv := sourceLinkFixture(t)
	selection := []InvoiceSourceSelection{{FileID: "file"}}
	if err := linkSelectedInvoiceSources(db, actor, inv, selection, "同一票据的纸质原件人工关联"); err != nil {
		t.Fatal(err)
	}
	if err := linkSelectedInvoiceSources(db, actor, inv, selection, "同一票据的纸质原件人工关联"); err != nil {
		t.Fatalf("重复选择应幂等：%v", err)
	}
	rows := sourceRows(t, db, inv.ID)
	if len(rows) != 1 {
		t.Fatalf("重复选择产生了重复来源行：%+v", rows)
	}
	if rows[0].ParseRunID != "" || rows[0].CandidateIndex != -1 {
		t.Fatalf("人工关联不得伪造解析运行：%+v", rows[0])
	}
	var manual struct {
		Method         string `json:"method"`
		Reason         string `json:"reason"`
		InvoiceVersion int64  `json:"invoice_version"`
	}
	if err := json.Unmarshal([]byte(rows[0].SourcesJSON), &manual); err != nil {
		t.Fatal(err)
	}
	if manual.Method != "MANUAL_LINK" || manual.Reason != "同一票据的纸质原件人工关联" || manual.InvoiceVersion != 1 {
		t.Fatalf("人工关联内容不符合读侧约定：%s", rows[0].SourcesJSON)
	}
	if rows[0].SourcesHash != parseSourceHash(rows[0].SourcesJSON) {
		t.Fatal("人工关联摘要与读侧规则不一致")
	}
	view := sourceViewMap(t, db, inv, rows[0])
	if view["source_kind"] != "MANUAL_LINK" || view["manual_reason"] != "同一票据的纸质原件人工关联" {
		t.Fatalf("读侧未还原人工关联：%v", view)
	}
	// 已有解析来源的原件不再叠加人工关联行，保持读侧一票一类的来源语义。
	index := 0
	if err := linkSelectedInvoiceSources(db, actor, inv, []InvoiceSourceSelection{{FileID: "file", RunID: "run", CandidateIndex: &index}}, "补录解析页区"); err != nil {
		t.Fatal(err)
	}
	if err := linkSelectedInvoiceSources(db, actor, inv, selection, "再次人工关联"); err != nil {
		t.Fatal(err)
	}
	parsed := 0
	for _, row := range sourceRows(t, db, inv.ID) {
		if row.ParseRunID != "" {
			parsed++
		}
	}
	if parsed != 1 {
		t.Fatalf("解析来源写入不正确：%+v", sourceRows(t, db, inv.ID))
	}
	if len(sourceRows(t, db, inv.ID)) != 2 {
		t.Fatalf("解析来源与人工关联应各保留一行：%+v", sourceRows(t, db, inv.ID))
	}
}

// TestInvoiceSourceSelectionRejectsForeignOrUnrelatedDocuments 验证跨租户、他人及未关联原件一律不可写入。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceSourceSelectionRejectsForeignOrUnrelatedDocuments(t *testing.T) {
	db, actor, inv := sourceLinkFixture(t)
	sourceSeedDocument(t, db, "other", "app", "foreign-doc", "foreign-file", "")
	sourceSeedDocument(t, db, "t", "other", "other-owner-doc", "other-owner-file", "")
	sourceSeedDocument(t, db, "t", "app", "unrelated-doc", "unrelated-file", "")
	for name, selection := range map[string]InvoiceSourceSelection{
		"跨租户原件": {FileID: "foreign-file"},
		"他人原件":  {FileID: "other-owner-file"},
		"未关联原件": {FileID: "unrelated-file"},
	} {
		sourceErrorCode(t, linkSelectedInvoiceSources(db, actor, inv, []InvoiceSourceSelection{selection}, "越权追加"), "RESOURCE_NOT_FOUND", 404)
		if len(sourceRows(t, db, inv.ID)) != 0 {
			t.Fatalf("%s 被写入来源行", name)
		}
	}
	mismatch := []InvoiceSourceSelection{{FileID: "file", DocumentID: "unrelated-doc"}}
	sourceErrorCode(t, linkSelectedInvoiceSources(db, actor, inv, mismatch, "文档标识不符"), "RESOURCE_NOT_FOUND", 404)
	// 解析后记录 invoice_ids 的候选原件与票据可关联，允许补充来源。
	sourceSeedDocument(t, db, "t", "app", "candidate-doc", "candidate-file", `,"invoice_ids":["invoice"]`)
	if err := linkSelectedInvoiceSources(db, actor, inv, []InvoiceSourceSelection{{FileID: "candidate-file"}}, "解析候选原件人工关联"); err != nil {
		t.Fatalf("可关联原件应允许补充来源：%v", err)
	}
	if len(sourceRows(t, db, inv.ID)) != 1 {
		t.Fatal("可关联原件未写入来源行")
	}
	// 带运行的选择必须同时给出候选序号，否则无法定位不可变坐标。
	other := domain.Invoice{ID: "invoice", TenantID: "t", Owner: "app", Version: 1, FileIDs: []string{"file"}}
	sourceErrorCode(t, linkSelectedInvoiceSources(db, actor, other, []InvoiceSourceSelection{{FileID: "file", RunID: "run"}}, "缺少候选序号"), "INVOICE_SOURCE_INVALID", 422)
}

// TestInvoiceSourceSelectionLimits 验证单次选择与单票来源行的受控上限。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceSourceSelectionLimits(t *testing.T) {
	db, actor, inv := sourceLinkFixture(t)
	selections := make([]InvoiceSourceSelection, 0, invoiceSourceSelectionLimit+1)
	for i := 0; i <= invoiceSourceSelectionLimit; i++ {
		selections = append(selections, InvoiceSourceSelection{FileID: "file"})
	}
	sourceErrorCode(t, linkSelectedInvoiceSources(db, actor, inv, selections, "超量选择"), "FILE_REFERENCE_LIMIT", 422)
	if len(sourceRows(t, db, inv.ID)) != 0 {
		t.Fatal("超量选择被部分写入")
	}
	sourceSeedDocument(t, db, "t", "app", "extra-doc", "extra-file", "")
	wide := inv
	wide.FileIDs = []string{"file", "extra-file"}
	batch := make([]store.InvoiceSource, 0, invoiceSourceRowLimit)
	for i := 0; i < invoiceSourceRowLimit; i++ {
		batch = append(batch, store.InvoiceSource{ID: fmt.Sprintf("seed-%04d", i), TenantID: "t", InvoiceID: inv.ID, DocumentID: "doc", ParseRunID: "run", CandidateIndex: i, SourcesHash: parseSourceHash(`[]`), SourcesJSON: `[]`})
	}
	if err := db.CreateInBatches(batch, 200).Error; err != nil {
		t.Fatal(err)
	}
	sourceErrorCode(t, linkSelectedInvoiceSources(db, actor, wide, []InvoiceSourceSelection{{FileID: "extra-file"}}, "超过来源上限"), "INVOICE_SOURCE_LIMIT", 422)
	index := 1
	if err := linkSelectedInvoiceSources(db, actor, wide, []InvoiceSourceSelection{{FileID: "file", RunID: "run", CandidateIndex: &index}}, "重复选择既有来源"); err != nil {
		t.Fatalf("上限处重复选择仍应幂等：%v", err)
	}
	if len(sourceRows(t, db, inv.ID)) != invoiceSourceRowLimit {
		t.Fatalf("幂等选择改变了来源规模：%d", len(sourceRows(t, db, inv.ID)))
	}
}

// TestAppendControlledInvoiceSourcesReceiptAndGuards 验证来源追加复用受控锁、原因、版本与回执约定。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAppendControlledInvoiceSourcesReceiptAndGuards(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	app := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.review", "expense.invoice.read"}}
	inv := domain.Invoice{ID: "invoice", TenantID: "t", Owner: "app", Version: 1, InvoiceType: "VAT", InvoiceNumber: "123", InvoiceDate: "2026-11-10", Total: 100, ReviewStatus: "CONFIRMED", FileIDs: []string{"file"}}
	raw, _ := json.Marshal(inv)
	if err := db.Create(&store.Invoice{ID: inv.ID, TenantID: "t", Owner: "app", Version: 1, ReservationState: "RESERVED", ReservationClaimID: "", ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	cc := &commandContext{actor: app, operation: "invoice.source.append", resourceID: inv.ID, input: CommandInput{CommandID: "append-1", ExpectedVersion: 1}}
	version := CommandInput{CommandID: "append-1", ExpectedVersion: 1}
	sourceErrorCode(t, appendInvoiceSourcesForTest(t, db, cc, InvoiceControlInput{CommandInput: version, FileIDs: []string{"file"}}), "REASON_REQUIRED", 422)
	sourceErrorCode(t, appendInvoiceSourcesForTest(t, db, cc, InvoiceControlInput{CommandInput: CommandInput{CommandID: "append-1", ExpectedVersion: 2}, FileIDs: []string{"file"}, Reason: "补充来源"}), "VERSION_CONFLICT", 409)
	sourceErrorCode(t, appendInvoiceSourcesForTest(t, db, cc, InvoiceControlInput{CommandInput: version, FileIDs: []string{"file"}, Reason: "补充来源"}, InvoiceControlInput{CommandInput: version, FileIDs: []string{"missing-file"}, Reason: "补充来源"}), "INVOICE_SOURCE_INVALID", 422)
	out, resource, err := appendControlledInvoiceSources(db, cc, InvoiceControlInput{CommandInput: version, FileIDs: []string{"file"}, Reason: "补充原件来源"})
	if err != nil {
		t.Fatalf("已占用票据受控追加来源失败：%v", err)
	}
	if resource != inv.ID {
		t.Fatalf("回执对象标识不正确：%s", resource)
	}
	receipt, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("回执结构不符合约定：%T", out)
	}
	if receipt["invoice_id"] != inv.ID || receipt["version"] != int64(1) {
		t.Fatalf("回执缺少票据标识或版本：%v", receipt)
	}
	items, _ := receipt["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("回执未返回写入的来源视图：%v", receipt["items"])
	}
	if items[0].(map[string]any)["source_kind"] != "MANUAL_LINK" {
		t.Fatalf("无运行追加应为显式人工关联：%v", items[0])
	}
	if len(sourceRows(t, db, inv.ID)) != 1 {
		t.Fatal("受控追加未写入唯一来源行")
	}
}

// appendInvoiceSourcesForTest 连续执行受控来源追加并返回最后一次错误，便于断言前置守卫。
// 参数：t 为测试上下文，db 为临时库，cc 为命令，inputs 为按顺序执行的请求。
// 返回值：最后一次执行错误或nil。
func appendInvoiceSourcesForTest(t *testing.T, db *gorm.DB, cc *commandContext, inputs ...InvoiceControlInput) error {
	t.Helper()
	var last error
	for _, input := range inputs {
		_, _, last = appendControlledInvoiceSources(db, cc, input)
	}
	return last
}

// TestInvoiceConfirmLinksParsedSourcesReadBack 验证确认命令写侧与读取侧对同一来源行的语义一致。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceConfirmLinksParsedSourcesReadBack(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "source-rule", InvoiceType: "VAT", IdentityFields: []string{"invoice_number"}, Active: true})
	if err := db.Create(&store.InvoiceParseRun{ID: "run-confirm", TenantID: "t", DocumentID: "fixture-file", JobID: "job-confirm", Attempt: 1, ParserVersion: "parser-v2", RulesVersion: "expense-v1", InputSHA256: "input", Status: "SUCCEEDED", ResultHash: "result", ResultJSON: `{"documents":[{"sources":[{"page":3,"x":1.5,"y":2.5,"width":10,"text":"税额"}]}]}`}).Error; err != nil {
		t.Fatal(err)
	}
	app := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.invoice.review", "expense.invoice.read"}}
	body := map[string]any{"command_id": "source-create", "invoice_type": "VAT", "invoice_number": "SRC-1", "invoice_date": "2026-11-10", "total_minor": "100", "file_ids": []string{"file"}}
	created := runCommand(t, s, app, "invoice.create", "", body)
	invoiceID, _ := created["id"].(string)
	body["command_id"] = "source-confirm"
	body["expected_version"] = 1
	body["reason"] = "人工核对原件后确认"
	body["source_selections"] = []any{map[string]any{"file_id": "file", "run_id": "run-confirm", "candidate_index": 0}}
	runCommand(t, s, app, "invoice.confirm", invoiceID, body)
	rows := sourceRows(t, db, invoiceID)
	if len(rows) != 1 {
		t.Fatalf("确认命令应写入单条解析来源：%+v", rows)
	}
	if rows[0].ParseRunID != "run-confirm" || rows[0].CandidateIndex != 0 || rows[0].DocumentID == "" {
		t.Fatalf("确认命令来源关系不正确：%+v", rows[0])
	}
	rawSources, err := s.InvoiceSources(context.Background(), app, invoiceID)
	if err != nil {
		t.Fatalf("读取侧拒绝确认命令的来源行：%v", err)
	}
	var read struct {
		Items   []map[string]any `json:"items"`
		FileIDs []string         `json:"file_ids"`
	}
	if err = json.Unmarshal(rawSources, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Items) != 1 || read.Items[0]["source_kind"] != "PARSED" || read.Items[0]["run_id"] != "run-confirm" {
		t.Fatalf("读取侧未还原确认来源：%s", rawSources)
	}
	if read.Items[0]["sources_hash"] != rows[0].SourcesHash {
		t.Fatal("确认命令写入的摘要与读取侧不一致")
	}
	sources, _ := read.Items[0]["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["text"] != "税额" {
		t.Fatalf("读取侧坐标未还原：%v", read.Items[0]["sources"])
	}
}

// TestManualSplitCreationKeepsSelectedCandidateSource 验证人工拆票创建时即可保留真实运行和页区来源。
// 参数：t 为独立临时库场景。
// 返回值：无。
func TestManualSplitCreationKeepsSelectedCandidateSource(t *testing.T) {
	db, actor, _ := sourceLinkFixture(t)
	if err := db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	actor.Permissions = append(actor.Permissions, "expense.invoice.upload")
	service := New(db, Dependencies{Files: filesFixture{}})
	result := runCommand(t, service, actor, "invoice.create", "", map[string]any{"command_id": "manual-split-source", "invoice_type": "VAT", "file_ids": []string{"file"}, "total_minor": "100", "reason": "人工拆分并绑定第二页候选", "source_selections": []map[string]any{{"document_id": "doc", "file_id": "file", "run_id": "run", "candidate_index": 1}}})
	rows := sourceRows(t, db, result["id"].(string))
	if len(rows) != 1 || rows[0].ParseRunID != "run" || rows[0].CandidateIndex != 1 {
		t.Fatalf("人工拆票来源丢失: %+v", rows)
	}
}
