# 报销管理子系统 · 系统 ER 图

> 来源：《报销管理子系统需求规格说明书 V1.1》· 第五章 数据模型（关键实体与字段）。
> 外系统实体（项目/任务、用户、部门）以"参考实体"形式给出，仅保留报销侧的主键与外键引用关系。

## 实体关系总览（Mermaid）

```mermaid
erDiagram
    PROJECT_TASK ||--o{ EXPENSE_REIMBURSEMENT : "关联(FR-1.2)"
    USER         ||--o{ EXPENSE_REIMBURSEMENT : "报销人"
    DEPARTMENT   ||--o{ EXPENSE_REIMBURSEMENT : "所属部门"
    EXPENSE_REIMBURSEMENT ||--|{ EXPENSE_ITEM        : "包含"
    EXPENSE_REIMBURSEMENT ||--|{ ALLOCATION_RECORD   : "多项目分摊(FR-1.6)"
    EXPENSE_REIMBURSEMENT ||--|{ APPROVAL_RECORD     : "审批留痕(FR-4)"
    EXPENSE_REIMBURSEMENT ||--o| PAYMENT_RECORD      : "付款(FR-5.1)"
    EXPENSE_ITEM         }o--o| INVOICE             : "关联发票(支持无票FR-3.3)"
    INVOICE              ||--|| INVOICE_IMAGE       : "原始影像(FR-2.9)"
    EXPENSE_ITEM         }o--|| EXPENSE_STANDARD    : "匹配标准(FR-2.6)"

    EXPENSE_REIMBURSEMENT {
        string expense_no PK "单号 · 规则自增(FR-3.2)"
        string user_id FK "报销人"
        string department_id FK "所属部门"
        string project_task_id FK "关联项目/任务(FR-1.2)"
        string expense_type "差旅/日常/项目执行(FR-3.2)"
        decimal total_amount "合计金额"
        string status "草稿/审批中/已付款/异常"
        boolean is_over_standard "是否超标(FR-2.7)"
        boolean is_travel_abnormal "是否差旅异常(FR-7.3)"
        string route_type "技术线/销售线(FR-4.1)"
        string full_path_flag "是否走全路径"
        datetime created_at "创建时间"
        datetime submitted_at "提交时间"
        datetime paid_at "付款时间"
    }

    EXPENSE_ITEM {
        string item_id PK "项ID"
        string expense_no FK "所属报销单"
        string fee_type "差旅交通/住宿/餐饮/市内交通/办公/通讯/其他"
        decimal amount "金额"
        boolean is_over_standard "超标标记(FR-2.6)"
        string over_reason "超标说明(FR-2.7)"
        string invoice_id FK "关联发票(可空,支持无票FR-3.3)"
        string standard_id FK "匹配的报销标准"
        string allocation_note "分摊备注"
    }

    INVOICE {
        string invoice_id PK "发票ID"
        string invoice_code "发票代码(FR-2.2)"
        string invoice_no "发票号码(FR-2.2)"
        date invoice_date "开票日期"
        decimal total_amount "价税合计"
        decimal tax_amount "税额"
        decimal amount_excl_tax "不含税金额"
        string buyer_name "购买方名称"
        string buyer_tax_no "购买方税号"
        string seller_name "销售方名称"
        string seller_tax_no "销售方税号"
        string goods_name "货物或应税劳务名称"
        string invoice_type "专票/普票/电子发票(FR-2.2)"
        string verify_result "一致/不一致/查无此票/超期(FR-2.3)"
        datetime verify_at "验真时间"
        boolean dup_flag "查重命中(FR-2.4)"
    }

    INVOICE_IMAGE {
        string image_id PK "影像ID"
        string invoice_id FK "关联发票(FR-2.9)"
        string file_url "原文件存储路径"
        string ocr_raw "OCR 原始结果(FR-2.2)"
        datetime uploaded_at "上传时间"
    }

    ALLOCATION_RECORD {
        string alloc_id PK "分摊记录ID"
        string expense_no FK "所属报销单"
        string project_task_id FK "分摊项目"
        decimal amount "分摊金额(FR-1.6)"
        decimal ratio "分摊比例"
    }

    APPROVAL_RECORD {
        string record_id PK "审批记录ID"
        string expense_no FK "所属报销单"
        string node_name "节点:团队负责人/技术总监/销售总监/财务总监/财务合规性/出纳"
        string approver_id FK "审批人"
        string action "通过/驳回/加签/转交/撤回(FR-4.2)"
        string comment "意见"
        datetime acted_at "时间"
    }

    PAYMENT_RECORD {
        string payment_id PK "付款记录ID"
        string expense_no FK "所属报销单(0..1)"
        string pay_method "银行转账/现金(FR-5.1)"
        string pay_serial_no "付款流水号(FR-5.1)"
        date pay_date "付款日期"
        decimal paid_amount "实付金额"
        string payee_id FK "收款人(报销人)"
    }

    EXPENSE_STANDARD {
        string standard_id PK "标准ID"
        string fee_type "费用类型(住宿/餐饮/交通… FR-8.1)"
        string city_tier "城市等级"
        string job_level "职级"
        decimal upper_limit "金额上限"
        date effective_from "生效时间"
        string config_by "配置人(系统管理员)"
    }

    PROJECT_TASK {
        string project_task_id PK "项目/任务ID · 引用主系统"
        string project_name "项目名称(冗余展示用)"
    }

    USER {
        string user_id PK "用户ID · 引用主系统"
        string user_name "姓名"
        string department_id FK "所属部门"
        string role "实施/PM/销售/团队负责人/技术总监/销售总监/财务总监/会计/出纳/管理员"
    }

    DEPARTMENT {
        string department_id PK "部门ID · 引用主系统"
        string dept_name "部门名称"
        string dept_type "技术部门/销售部门/财务部门"
    }
```

> 注：审批留痕由主系统基础平台审计能力统一提供（参见 FR-4.7 已移除），`APPROVAL_RECORD` 实体保留为业务视图，实际存储可对接基础平台。

## 关键设计说明

### 1. 主实体
- **EXPENSE_REIMBURSEMENT** 是聚合根，承载一张报销单的完整生命周期。
- `route_type`（技术线 / 销售线）+ `full_path_flag`（是否走全路径）共同驱动 FR-4.1 条件路由。
- `is_over_standard` / `is_travel_abnormal` 由费用项匹配（FR-2.6）与差旅比对（FR-7.2）汇总得到。

### 2. 关系约束
| 关系 | 基数 | 业务约束 |
| --- | --- | --- |
| 报销单 → 费用项 | 1 : N | 至少 1 条（保存草稿时可暂为空） |
| 报销单 → 分摊记录 | 1 : N | 各分摊金额合计 = 报销单合计（BR-5） |
| 报销单 → 审批记录 | 1 : N | 留痕所有审批动作（FR-4.2） |
| 报销单 → 付款记录 | 1 : 0..1 | 付款前为空，付款后落 1 条（FR-5.1） |
| 费用项 → 发票 | N : 0..1 | 支持无票/预付（FR-3.3，须说明） |
| 发票 → 发票影像 | 1 : 1 | 影像与结构化数据一一绑定（FR-2.9） |
| 费用项 → 报销标准 | N : 1 | 按费用类型 + 城市等级 + 职级匹配（FR-2.6/8.1） |

### 3. 跨系统引用
- `PROJECT_TASK / USER / DEPARTMENT` 三个实体由主系统统一管理，本子系统**只读引用**主键，不在本子系统内维护其主数据。
- 字段级访问控制：报销人仅可见本人单据；项目经理/团队负责人可见所辖项目；财务可见金额与发票明细，不可见技术细节（见需求文档第 2 章 字段级访问控制）。

### 4. 归档与不可变
- 报销单结案（已付款）后，连同发票影像、审批记录、付款记录由 **FR-6.1 自动归集** 至项目归档包，转为只读（FR-6.3），不再参与日常 CRUD。
