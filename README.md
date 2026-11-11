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

## 结算成本候选只读用途

独立机器入口`GET /internal/v1/settlement/cost-candidates`只接受`settlement_reimbursement_cost_read`用途的精确`reimbursement.cost_candidate.read` scope，不复用项目/文件/审计凭据或浏览器会话。`deploy/reimbursement.env.example`中`REIMBURSEMENT_COST_MACHINE_ENABLED=false`默认关闭，平台独立注册与下发完成后才可启用；缺字段或错tenant/app/env/issuer/audience配置启动失败。沿平台真实协议：JWT sub是OAuth client label，oauth_client_id是注册UUID，token_use=application，aud为单项字符串，scope为单项数组，最长900秒。

入口可用claim_id精确查询；不分页，单只读RR事务返回完整有界集合，最多1000单、10000冻结分摊行、8MiB，超限整体失败。每个不可变付款/冲正effect按批准内容hash连接当时审批快照，frozen_items保留稳定item_id/allocation_key、expense_nature、发生日、精确整数分摊、提交组织/申请人、凭证摘要。当前草稿修改不会污染历史；旧批准快照或费用性质缺失时显式UNKNOWN，仍保留真实付款效果，不猜造成本。

实际费用ACTUAL与预付PREPAYMENT是逐item的expense_nature，可同单混合；ClaimType是TRAVEL/DAILY/PROJECT_EXECUTION，不能推断预付。候选不是成本确认，结算须真实凭证与独立复核；原确认存在未处置冲正差异时同单新付款确认冻结，稳定单据/费用项/分摊额度不因重付重置。报销详情可跳转结算的收入与成本确认入口，结算仍独立检查角色动作和ORG/TENANT范围。
