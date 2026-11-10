# 报销管理系统

按《报销管理系统完整开发方案V1.0.docx》开发的Go独立业务服务，前端位于工作区`frontend/src/modules/reimbursement`。

技术框架参考合同与项目系统：Go、Gin、GORM、MySQL；审批/付款由本地事务和不可变账本维护，后台投递使用数据库租约及outbox。

```sh
go test ./...
go vet ./...
go build ./cmd/...
sh scripts/license-core-sync.sh --check
```

`cmd/api`、`cmd/worker`、`cmd/migrate`、`cmd/authz-catalog`为独立入口。配置模板见[运行配置](deploy/reimbursement.env.example)，操作及恢复说明见[运行手册](docs/runbooks/operations.md)，复用来源见[身份基础来源](docs/architecture-reuse.md)。真实配置、凭据和业务数据不进入源码。

实现状态、实际验证与未完成的跨系统验收以工作区`.ai/qa/reimbursement-development-20261110/TASK_STATE.md`和对应报告为准；源码存在不等于已发布或验收通过。
