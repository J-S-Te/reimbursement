package jobs

import (
	"context"
	"encoding/json"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"testing"
)

// TestArchiveActionDetailsKeepActualParentKind 验证普通审批资格证明不会被清单误称为转交。
// 参数：t为隔离归档测试上下文。
// 返回值：无。
func TestArchiveActionDetailsKeepActualParentKind(t *testing.T) {
	f := newArchiveStateFixture(t)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var row struct{ ManifestJSON string }
	if err := f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		History map[string]json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal([]byte(row.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.History["approval_transfers"]; exists {
		t.Fatal("新清单仍把所有动作证明称为转交")
	}
	var details []struct {
		ActionID string `json:"ActionID"`
	}
	if err := json.Unmarshal(manifest.History["approval_action_details"], &details); err != nil {
		t.Fatal(err)
	}
	if len(details) != 1 || details[0].ActionID != "kept-action" {
		t.Fatal("普通动作证明丢失父记录")
	}
}
