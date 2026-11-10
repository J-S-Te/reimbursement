package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

type ProjectReferences interface {
	Validate(context.Context, domain.Actor, string, int64, []domain.ReferenceRequest) ([]domain.ProjectReference, error)
	Search(context.Context, domain.Actor, string, string, int) (json.RawMessage, error)
}
type FileGateway interface {
	Validate(context.Context, domain.Actor, []string, string) error
	Download(context.Context, domain.Actor, string) (json.RawMessage, error)
}
type Personnel interface {
	VerifyApprover(context.Context, domain.Actor, string, string) error
}

type Dependencies struct {
	Personnel   Personnel
	Projects    ProjectReferences
	Files       FileGateway
	Environment string
}
type Service struct {
	db   *gorm.DB
	deps Dependencies
}

// New 创建报销应用服务，缺少的远端适配器在需要时失败关闭。
// 参数：db 为独立报销库；dependencies 为可信项目、文件与环境依赖。
// 返回值：负责事务编排的应用服务。
func New(db *gorm.DB, dependencies Dependencies) *Service {
	return &Service{db: db, deps: dependencies}
}

// UploadGateway 提供带本地资源归属的文件上传生命周期。
type UploadGateway interface {
	CreateUpload(context.Context, domain.Actor, string, json.RawMessage) (json.RawMessage, error)
	CompleteUpload(context.Context, domain.Actor, string) (json.RawMessage, error)
	ReadDocument(context.Context, string, string) ([]byte, error)
}
