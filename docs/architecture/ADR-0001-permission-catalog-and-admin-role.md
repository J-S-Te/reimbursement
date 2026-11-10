# ADR-0001：权限目录版本与 reimbursement_admin 超级管理员角色

| 项 | 内容 |
|---|---|
| 状态 | 已接受（Accepted） |
| 决策日期 | 2026-11-10 |
| 决策人 | 用户（业务负责人） |
| 记录范围 | 报销权限目录、平台目录发布、V1.0 方案 §4.2 偏差追溯 |

## 1. 背景

《报销管理系统完整开发方案V1.0.docx》§4.2「权限目录与业务数据范围」的默认设计是：`reimbursement_admin` 对应业务配置管理员，默认只管理配置、接入诊断和受控任务修复，不自动取得审批、付款或全量财务阅读权；完整权限码表在 `authz/permission-manifest.yaml` 固化。

2026-11-10 岗位对齐时，用户明确“配置管理员改超级管理员”，要求该稳定角色码保留全部业务能力，并单独持有本租户全量范围能力。当前目录实际状态为：

- `catalog_version=3`，9 个角色、31 条权限（`reimbursement/authz/permission-manifest.yaml`）。
- `reimbursement_admin` 显示名为“超级管理员”，持有全部 31 条权限，包括 `expense.approval.act`、`expense.payment.record`、`expense.payment.reversal.approve`、`expense.audit.read` 以及唯一一份 `expense.scope.tenant_all`。
- `reimbursement/authz/manifest_test.go` 对 3/9/31、稳定角色码、显示名、全量动作和 `tenant_all` 唯一性作出断言。

岗位与超级管理员规则以 `.ai/qa/reimbursement-role-alignment-20261110/` 为准。该检查点记录的是 v2/29 口径；现行目录为 v3/31，相差的两项为恢复类能力 `expense.approval.recover` 与 `expense.integration.manage`。目录版本、角色数和权限数一律以 manifest 及 `manifest_test.go` 为准。上述 v2→v3 计数漂移不改变“保留全量权限”这一决策，本 ADR 也不回写历史检查点。

## 2. 决策

1. 保留 `reimbursement_admin` 超级管理员的全量业务权限（31/31）及本租户 `expense.scope.tenant_all` 范围能力。
2. 首发不采纳拆角色方案（不拆分“配置管理员/审批管理员/付款管理员/审计只读管理员”等角色）。
3. 保持稳定角色码 `reimbursement_admin`，仅显示名使用“超级管理员”，避免平台目录、历史授权和代码映射漂移。
4. 岗位与超级管理员规则以 role-alignment 文档为准；权限码表、目录版本和计数以 `reimbursement/authz/permission-manifest.yaml` 与 `reimbursement/authz/manifest_test.go` 为准。
5. 超级管理员仍受租户边界、职责分离、任务受理和状态机约束；这些约束是运行时强制校验，不因目录授予全量动作而放宽。

## 3. 理由与补偿控制

理由：首发阶段需要单一运维入口完成配置、接入诊断和受控任务修复；超级管理员若拆成多个角色，会扩大目录发布、初始授权、权限撤销和故障回退的组合面，且平台初始角色映射若依赖默认策略还可能回退为 admin。用户在 2026-11-10 明确选择保留全量权限，并接受以运行时职责分离作为补偿控制。

补偿控制（路径相对 `reimbursement/`，仅记录现行代码位置）：

- 自审阻断：`internal/application/submit.go:405-416` 生成审批路线时拒绝申请人与审批人重叠（`SELF_APPROVAL_FORBIDDEN`）；`internal/application/approval.go:45-46` 办理任务时再次要求当前受理人独立于申请人。
- 自付阻断：`internal/application/payments.go:88-89` 申请人不能为本人登记付款（`SELF_PAYMENT_FORBIDDEN`）。
- 会计/出纳分离：`internal/application/payments.go:91-96` 登记付款前检查本单本轮是否存在同一操作人的会计通过记录（`ACCOUNTANT_CASHIER_CONFLICT`）。
- 冲正独立批准：`internal/application/payments.go:311-312` 申请人不能申请冲正本人付款（`SELF_REVERSAL_FORBIDDEN`）；`internal/application/payments.go:345-346` 批准人必须独立于申请人与原付款人（`REVERSAL_APPROVER_CONFLICT`）；申请与批准分别使用 `expense.payment.reversal.request` 与 `expense.payment.reversal.approve`（`internal/application/payments.go:304-309`）。
- 任务受理人约束：`internal/application/approval.go:45-46` 只有任务当前 `assignee` 且非申请人可办理，并要求具备任务所需动作权限。
- 租户隔离：`internal/application/commands.go:82-97` 的 `loadClaim` 按 `tenant_id + id` 读取；`internal/application/access.go:105-107` 的 `requireFinancial` 拒绝租户不匹配；任务、付款、冲正查询均带 `tenant_id`。
- 财务数据范围：`internal/application/access.go:30-56` 的 `financeScope` 只认显式 `tenant_all` 或本地发布组织范围，不从角色名或平台 APPLICATION grant 推导。

以上控制是当前源码事实。本决策记录不声称这些控制已在真实平台、真实租户或浏览器场景完成验收。

## 4. 风险接受与复审触发条件

接受的风险：单角色全量权限放大了误操作、凭据泄露和内部越权的后果；安全边界依赖运行时职责分离与审计，而不是目录最小权限；审计追溯需要按实际动作人而非角色名判断责任。

出现以下任一情况必须复审本决策，并更新本 ADR 或新增 ADR：

1. 平台侧可通过角色组合（如申请人+审批人、会计+出纳、付款登记+冲正批准）组合出与超级管理员等价的能力，或平台提供更细粒度角色映射。
2. 新增高权限操作或数据范围，例如新审批节点、付款/冲正动作、全量财务读取、跨租户能力、配置发布能力。
3. 多租户客户、等保/审计或合同要求最小权限交付。
4. 职责分离校验被移除、放宽，或相关测试失败、被跳过。
5. 用户或业务负责人重新提出拆分角色。

复审输出必须同步 manifest、`manifest_test.go`、模块文档与运维手册；不得只改代码或只改文档造成口径再次漂移。

## 5. 影响面

- 权限目录版本策略：`catalog_version` 是发布契约。任何权限、角色或范围变更都须递增版本，并同步 `authz/manifest_test.go` 的 3/9/31 断言、`.ai/modules/reimbursement.md`、运维手册和平台兼容窗口。不得静默修改目录而不升版。
- 平台目录发布窗口：先发布项目目录 v11（含最小 `sales_director`）与报销目录 v3，再分配 `reimbursement_admin` 及各业务角色；目录发布不依赖 API 已就绪（`reimbursement-authz-catalog publish`）。发布后核对 `catalog_version`、角色数、权限数及 `tenant_all` 唯一性。
- 方案 §4.2 偏差记录：V1.0 §4.2 的“业务配置管理员仅配置/诊断/修复”最小权限表述与当前实现不一致，本 ADR 是该偏差的正式记录。V1.0 docx 为只读业务基线，不修改原文；后续方案修订应在 §4.2 引用本 ADR。
- 文档口径：`.ai/modules/reimbursement.md` 与 `reimbursement/docs/runbooks/operations.md` 已按 v3/31 更正；role-alignment 检查点保留 v2/29 作为当时证据，不回写历史。

## 6. 参考

- `reimbursement/authz/permission-manifest.yaml`（JSON 内容、`.yaml` 扩展名）
- `reimbursement/authz/manifest_test.go`
- `.ai/qa/reimbursement-role-alignment-20261110/TASK_STATE.md`、`plan.md`、`backend-role-report.md`
- `.ai/qa/reimbursement-completion-20261110/plan.md`
- `reimbursement/报销管理系统完整开发方案V1.0.docx` §4.2（只读）
- `reimbursement/internal/application/submit.go`、`approval.go`、`payments.go`、`access.go`、`commands.go`

## 变更追踪

- 2026-11-10（本任务）：目录由 v3/31 升至 **v4/32**，新增 `expense.invoice.correct`（独立批准票据重大更正，方案 §7.4/§7.5 的强制控制所需），仅授予 `reimbursement_admin` 与 `expense_accountant`；`claims_role_config_hash` 同步为 `reimbursement-v4-controlled-recovery`。
  本文档第 16-24、62-65 行中的 v3/9/31 为**决策当时**的目录口径，保留作历史依据；现行口径以 `authz/permission-manifest.yaml` 与 `authz/manifest_test.go` 的 4/9/32 断言为准。
  v4 升版要求平台侧重新发布报销授权目录，旧实例处于 N-1 兼容窗口，属发布协调项。
