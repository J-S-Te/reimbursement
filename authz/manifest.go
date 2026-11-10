package authz

import _ "embed"

// PermissionManifest 保存可独立发布的报销权限目录。
//
//go:embed permission-manifest.yaml
var PermissionManifest []byte
