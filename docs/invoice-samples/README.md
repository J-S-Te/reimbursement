# 发票样本需求：机器可读规格（给 AI 用）

> 版本 1.0.0 · 2026-11-10 · 适用：报销子系统发票解析验收（对应 `parser.authorized_samples`）

## 这是什么

一份**声明式规格 + 可执行校验器**，让人或 AI 都能判定"样本够不够、金标准对不对、验收过不过"。
规格是唯一权威，正文解释只在 `README.md`；两者冲突时以 `spec.v1.json` 为准。

| 文件 | 作用 |
|---|---|
| `spec.v1.json` | 权威规格：能力边界、48 份样本计划、命名、金标准列、合规、9 项验收指标、生命周期、AI 协议 |
| `spec.schema.json` | 规格自身的 JSON Schema（可校验 spec 是否被改坏） |
| `validate_samples.py` | 校验器：清点覆盖、检查金标准、计算 9 项指标、输出 report.json 与缺失清单 |
| `parse_report.template.json` | 解析结果输入的模板（3 行示例），定义 M2–M6 的输入形态 |
| `parse_report.schema.json` | 解析结果 JSON Schema，防止 AI 编造字段 |
| `score_parse_results.py` | 把"解析输出 + gold.csv"自动比对成 parse_report.json（免手工打分） |
| `发票样本收集表-给财务.xlsx` | 给财务的一页表：说明 / 收集清单（21 类）/ 金标准（48 行预填文件名）/ 命名规则 / 验收口径 |
| `README.md` | 本文件，人读说明 + 可直接粘贴给其他 AI 的提示词 |

## 快速开始

```bash
# 1. 把样本与金标准放到工作区外（推荐）或已 gitignore 的目录
mkdir -p ~/reim-samples
# 2. 放入：<ticket>-<seq>-<variant>.pdf|ofd|png、gold.csv、manifest.json
# 3. 跑校验
python3 docs/invoice-samples/validate_samples.py \
  --samples ~/reim-samples \
  --gold ~/reim-samples/gold.csv
# 4. 读取 ~/report.json（verdict + missing + metrics）
```

退出码：`0` 通过（ACCEPTED 或 VALIDATED_PENDING_PARSE）、`1` 阻塞项未通过、`2` 参数/文件错误。

## 样本计划速览（共 48 份）

| 批次 | 必须 | 数量 | 内容 |
|---|---|---|---|
| P0 | 是 | 25 | 专票/普票/数电票/OFD/行程单/铁路（**必须有文字层或 OFD**） |
| ADV | 是 | 12 | 扫描件、拍照、加密、损坏、超限、合并、相似税号、多税率 |
| P1 | 否 | 11 | 出租车、过路费、财政票据、境外、红冲、作废 |

能力的硬边界（写在 spec 里，不要臆测）：单文件 20MiB、展开 100MiB、100 页；**扫描件与图片没有 OCR，只能验证"失败进人工"**。

## AI 执行协议（7 步）

1. 读取 `spec.v1.json` 与 `manifest.json`，确认 `owner_approval=true`；否则停止并上报。
2. 按 `naming.pattern` 扫描目录；无法匹配的文件进 `unmapped`，**不计覆盖**。
3. 统计每个 case 的 `have/need`，计算 M1 覆盖，按 batch.owner 输出缺失清单。
4. 校验 `gold.csv`：必填列、整数分、数电票 20 位、file_name 与文件一一对应（M7）。
5. 若有解析结果，按 `parse_report.json` 计算 M2–M6/M9；**没有就标 NOT_EVALUATED，禁止推断**。
6. 输出 `report.json` + `summary.md` + `missing.csv`，给出 verdict。
7. 任一阻塞项未通过 → REJECTED，不得写 ACCEPTED；补齐后从第 2 步重跑。

## 解析结果如何接入（M2–M6/M9）

解析跑完后，用 scorer 把结果对成校验器认识的格式：

```bash
# 1. 解析器输出 parsed.json（每票一条）：
#    {"results":[{"file_name":"SPECIAL-001-normal.pdf","status":"PARSED",
#      "auto_confirmed":true,"manual_reviewed":false,"crashed":false,
#      "fields":{"invoice_number":"...","seller_tax_id":"...","buyer_tax_id":"...","total_minor":"12345"}}]}
# 2. 比对金标准，生成 parse_report.json
python3 docs/invoice-samples/score_parse_results.py \
  --spec docs/invoice-samples/spec.v1.json \
  --gold ~/reim-samples/gold.csv \
  --parsed ~/reim-samples/parsed.json \
  --out ~/reim-samples/parse_report.json
# 3. 带解析结果再校验（此时才会计算 M2–M6/M9）
python3 docs/invoice-samples/validate_samples.py \
  --samples ~/reim-samples --gold ~/reim-samples/gold.csv \
  --parse-report ~/reim-samples/parse_report.json
```

关键口径：
- `auto_parsed_ok` = `status=PARSED` **且** 4 个关键字段全对 **且** 未崩溃；
- `auto_confirmed=true` 但关键字段错 → **M3 阻断（0 容忍）**；
- 扫描件/图片被标成 `PARSED` → **M4 阻断**；
- 解析结果缺失的文件记 `MISSING`，不计入成功，不得推断；
- 没有 `parse_report` 时，阻塞项未评估 → 结论只能是 `VALIDATED_PENDING_PARSE`，不会是 ACCEPTED。

`manifest.json` 必填三项：`owner_approval`、`redaction_reviewed_by`、`redaction_violations`。
缺少脱敏签字时 M8 未评估，同样只能停在 PENDING。

## 判定规则

| verdict | 含义 | 下一步 |
|---|---|---|
| `ACCEPTED` | 阻塞项全过 + 人工签字 | 关闭 `parser.authorized_samples` |
| `VALIDATED_PENDING_PARSE` | 样本与金标准齐了，但尚无解析结果 | 跑解析后重算 M2–M6 |
| `REJECTED` | 缺样本或阻塞项失败 | 按 missing.csv 补样，或修解析 |
| 停止条件命中 | 无授权/无金标准/发现未脱敏 | 立即停，找评审人 |

## 谁能决定什么

| AI 可以判定 | 必须人来决定 |
|---|---|
| 覆盖是否达标、文件如何分类 | 真实票据是否可用于测试 |
| 指标计算、verdict、缺失清单 | 脱敏是否充分并签字 |
| 拒绝理由与最小补样集合 | 最终 ACCEPTED 签收 |
| 报告与证据索引 | 改阈值、加票种、改首发范围 |

AI **禁止**：把人工结果写成自动成功、用合成文件冒充样本、为通过而改阈值、在阻断未过时给 ACCEPTED。

## 扩展一个新票种

1. 在 `sample_plan.batches[*].cases` 增加一条：`code/ticket/variant/media/count/expected/blocking`。
2. 若需要新金标准列，先改 `gold_standard.columns`（AI 不得私自放宽必填列）。
3. 跑 `spec.schema.json` 校验 + 用 2–3 份新样本试跑 `validate_samples.py`。
4. 更新 `total_required_files`。

## 直接粘贴给其他 AI 的提示词

```text
你是报销子系统发票样本验收执行器。规格文件：docs/invoice-samples/spec.v1.json
步骤：读取 spec 与样本目录 → 按 naming.pattern 分类 → 计算每个 case 的覆盖与 M1 →
校验 gold.csv 的必填列/整数分/数电票20位/一一对应 → 若有 parse_report 计算 M2-M6 →
输出 report.json、summary.md、missing.csv → 给出 verdict。
硬约束：未匹配文件不计覆盖；无解析结果标 NOT_EVALUATED；阻塞项不过不得给 ACCEPTED；
不得修改 spec 阈值；发现未脱敏立即停止并上报。
```

## 边界与不做的事

- 不覆盖真实平台 OIDC/文件网关联调、目标 MySQL 8.4、浏览器全链路（见 V1.0 §21 其他用例）。
- 不提供 OCR；扫描件与图片只验失败安全。
- 外部税务验真供应商未接入，样本不用于验真联调。
- 样本原文件**不得提交进 Git**；本目录只放规格与工具。
