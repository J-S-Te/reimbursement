package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
)

// TestArchiveBundleRetainsManifestAndChecksOriginalHash 验证归档文件包携带封存清单且逐份校验原件。
// 参数：t 为独立最小档案包场景。
// 返回值：无。
func TestArchiveBundleRetainsManifestAndChecksOriginalHash(t *testing.T) {
	original := []byte("authorized original")
	hash := sha256.Sum256(original)
	manifest, _ := json.Marshal(map[string]any{"package_version": 1, "files": []map[string]string{{"file_id": "file", "document_id": "doc", "sha256": hex.EncodeToString(hash[:])}}})
	content, err := buildArchiveBundle(context.Background(), manifest, func(context.Context, string) ([]byte, error) { return original, nil })
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil || len(reader.File) != 2 {
		t.Fatalf("归档包文件不完整: %v", err)
	}
	first, _ := reader.File[0].Open()
	data, _ := io.ReadAll(first)
	first.Close()
	if !bytes.Equal(data, manifest) {
		t.Fatal("封存清单被修改")
	}
	if _, err = buildArchiveBundle(context.Background(), manifest, func(context.Context, string) ([]byte, error) { return []byte("changed"), nil }); err == nil {
		t.Fatal("原件哈希变化仍生成包")
	}
}
