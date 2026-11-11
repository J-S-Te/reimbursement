#!/usr/bin/env python3
"""报销发票样本清单校验器（AI 可执行）。

用法：
  python3 validate_samples.py --samples ~/reim-samples --gold ~/reim-samples/gold.csv
  python3 validate_samples.py --samples ... --parse-report parse_report.json --json

退出码：
  0  最高门槛通过（阻塞项全过）
  1  阻塞项未通过 / 存在停止条件
  2  参数或文件错误

只做机器可判定的检查：覆盖、命名、金标准完整性、脱敏标记、解析结果打分。
不做的事情：不修改样本、不推断缺失、不把人工结果当自动成功。
"""

import argparse
import csv
import json
import os
import re
import sys
from collections import defaultdict

STOP = []


def fail(msg):
    STOP.append(msg)


def load_json(path, what):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception as exc:  # noqa: BLE001
        print("ERROR: 无法读取" + what + ": " + str(exc), file=sys.stderr)
        sys.exit(2)


def guess_media(ext):
    ext = ext.lower().lstrip(".")
    if ext == "pdf":
        return "pdf"
    if ext == "ofd":
        return "ofd"
    if ext in ("jpg", "jpeg", "png"):
        return "image"
    return "unknown"


def media_compatible(ext_media, case_media):
    """case.media 可能是 pdf_text|ofd 这种组合；pdf 同时兼容 pdf_text 与 pdf_image。"""
    allowed = set(case_media.split("|"))
    if ext_media == "pdf":
        return bool(allowed & {"pdf_text", "pdf_image"}) or "pdf" in allowed
    return ext_media in allowed


def match_case(spec, ticket, variant, ext_media):
    hits = []
    for batch in spec["sample_plan"]["batches"]:
        for case in batch["cases"]:
            if case["ticket"] != ticket:
                continue
            if case["variant"] != variant:
                continue
            if media_compatible(ext_media, case["media"]):
                hits.append((batch, case))
    return hits


def scan_samples(spec, root):
    pattern = re.compile(spec["naming"]["pattern"])
    per_case = defaultdict(list)
    unmapped = []
    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            if name in (spec["naming"]["gold_file"], spec["naming"]["manifest_file"]):
                continue
            if name.startswith("."):
                continue
            ext = name.rsplit(".", 1)[-1].lower() if "." in name else ""
            if ext not in ("pdf", "ofd", "jpg", "jpeg", "png"):
                continue
            m = pattern.match(name)
            if not m:
                unmapped.append(name)
                continue
            ticket = m.group("ticket")
            variant = m.group("variant")
            ext_media = guess_media(m.group("ext"))
            hits = match_case(spec, ticket, variant, ext_media)
            if not hits:
                unmapped.append(name)
                continue
            for batch, case in hits:
                per_case[case["code"]].append(os.path.join(dirpath, name))
    return per_case, unmapped


def check_gold(spec, gold_path, submitted_files):
    result = {"present": False, "rows": 0, "errors": [], "missing_rows": [], "extension_mismatch": []}
    if not gold_path or not os.path.isfile(gold_path):
        fail("gold.csv 缺失")
        result["errors"].append("gold.csv 缺失")
        return result
    result["present"] = True
    spec_cols = spec["gold_standard"]["columns"]
    required = [c["name"] for c in spec_cols if c.get("required")]
    key_cols = spec["gold_standard"]["key_columns"]
    with open(gold_path, newline="", encoding="utf-8-sig") as f:
        reader = csv.DictReader(f)
        header = reader.fieldnames or []
        for col in required:
            if col not in header:
                result["errors"].append("缺少必填列: " + col)
        rows = list(reader)
    result["rows"] = len(rows)
    names = set()
    for i, row in enumerate(rows, start=2):
        name = (row.get("file_name") or "").strip()
        if not name:
            result["errors"].append("第" + str(i) + "行 file_name 为空")
            continue
        names.add(name)
        for col in required:
            if not (row.get(col) or "").strip():
                result["errors"].append("第" + str(i) + "行缺少必填字段 " + col)
        total = (row.get("total_minor") or "").strip()
        if total and not re.fullmatch(r"-?[0-9]+", total):
            result["errors"].append("第" + str(i) + "行 total_minor 必须是整数分：" + total)
        number = (row.get("invoice_number") or "").strip()
        if number and not number.isdigit():
            result["errors"].append("第" + str(i) + "行 invoice_number 必须为数字：" + number)
        if (row.get("invoice_type") or "") == "DIGITAL" and len(number) != 20:
            result["errors"].append("第" + str(i) + "行数电票号码应为20位，实际 " + str(len(number)))
    submitted_names = {os.path.basename(p) for p in submitted_files}
    result["missing_rows"] = sorted(submitted_names - names)
    if result["missing_rows"]:
        result["errors"].append("以下文件缺少金标准行: " + ", ".join(result["missing_rows"][:5]))
    return result


def load_parse_report(path):
    if not path:
        return None
    if not os.path.isfile(path):
        print("WARN: parse report 不存在，M2-M6/M9 标记 NOT_EVALUATED", file=sys.stderr)
        return None
    return load_json(path, "parse report")


def score(spec, parse_report, per_case, p0_codes):
    metrics = {}
    if parse_report is None:
        for m in spec["acceptance"]["metrics"]:
            if m["id"] != "M1-coverage" and m["id"] != "M7-gold-coverage":
                metrics[m["id"]] = {"status": "NOT_EVALUATED"}
        return metrics
    rows = parse_report.get("results", [])
    p0_rows = [r for r in rows if r.get("case_code") in p0_codes]
    auto_ok = sum(1 for r in p0_rows if r.get("auto_parsed_ok") is True)
    metrics["M2-auto-success"] = {"status": "EVALUATED", "value": (auto_ok / len(p0_rows)) if p0_rows else 0.0}
    metrics["M3-key-field-false-confirm"] = {
        "status": "EVALUATED",
        "value": sum(1 for r in rows if r.get("auto_confirmed") and r.get("key_fields_correct") is False),
    }
    metrics["M4-image-false-success"] = {
        "status": "EVALUATED",
        "value": sum(1 for r in rows if r.get("manual_only_media") and r.get("auto_parsed_ok")),
    }
    metrics["M5-limit-safety"] = {"status": "EVALUATED", "value": sum(1 for r in rows if r.get("crashed"))}
    metrics["M6-merged-silent-loss"] = {
        "status": "EVALUATED",
        "value": sum(1 for r in rows if r.get("case_code") == "ADV-MERGED" and r.get("silent_loss")),
    }
    metrics["M9-manual-rate"] = {
        "status": "EVALUATED",
        "value": sum(1 for r in p0_rows if r.get("manual_reviewed")) / len(p0_rows) if p0_rows else 0.0,
    }
    return metrics


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--spec", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "spec.v1.json"))
    ap.add_argument("--samples", required=True)
    ap.add_argument("--gold", default=None)
    ap.add_argument("--parse-report", default=None)
    ap.add_argument("--json", action="store_true", help="只输出 report JSON")
    args = ap.parse_args()

    spec = load_json(args.spec, "spec")
    if not os.path.isdir(args.samples):
        print("ERROR: 样本目录不存在: " + args.samples, file=sys.stderr)
        sys.exit(2)
    manifest_path = os.path.join(args.samples, spec["naming"]["manifest_file"])
    manifest = load_json(manifest_path, "manifest") if os.path.isfile(manifest_path) else None
    if manifest is None:
        fail("manifest.json 缺失（授权与 owner 证据）")
    elif not manifest.get("owner_approval"):
        fail("manifest.json 缺少 owner_approval 授权确认")

    per_case, unmapped = scan_samples(spec, args.samples)
    submitted = [p for files in per_case.values() for p in files]

    coverage = []
    missing = []
    p0_codes = set()
    for batch in spec["sample_plan"]["batches"]:
        for case in batch["cases"]:
            have = len(per_case.get(case["code"], []))
            need = case["count"]
            ok = have >= need
            if batch["id"] == "P0":
                p0_codes.add(case["code"])
            coverage.append({
                "batch": batch["id"], "code": case["code"], "label": case.get("label", ""),
                "need": need, "have": have, "ok": ok, "required": batch["required"],
            })
            if batch["required"] and not ok:
                missing.append({
                    "batch": batch["id"], "code": case["code"], "label": case.get("label", ""),
                    "need": need, "have": have, "gap": need - have, "owner": batch["owner"],
                })

    gold = check_gold(spec, args.gold, submitted)
    parse_report = load_parse_report(args.parse_report)
    metrics = score(spec, parse_report, per_case, p0_codes)

    # M1 覆盖与 M7 金标准由清点结果直接给出，其余来自 parse report。
    required_rows = [row for row in coverage if row["required"]]
    required_ok = all(row["ok"] for row in required_rows)
    metrics["M1-coverage"] = {
        "status": "EVALUATED",
        "value": (sum(1 for row in required_rows if row["ok"]) / len(required_rows)) if required_rows else 0.0,
    }
    metrics["M7-gold-coverage"] = {
        "status": "EVALUATED" if gold["present"] else "NOT_EVALUATED",
        "value": 0.0 if (gold["errors"] or gold["missing_rows"]) else 1.0,
    }
    reviewed_by = (manifest or {}).get("redaction_reviewed_by")
    metrics["M8-redaction"] = {
        "status": "EVALUATED" if reviewed_by else "NOT_EVALUATED",
        "value": (manifest or {}).get("redaction_violations", 0) if reviewed_by else None,
    }

    # 按 spec 阈值判定每项指标；阻塞项未评估则不得视为通过。
    failed_blocking = []
    pending_blocking = []
    acceptance = []
    for item in spec["acceptance"]["metrics"]:
        got = metrics.get(item["id"], {"status": "NOT_EVALUATED"})
        item["value"] = got.get("value")
        item["status"] = got.get("status", "NOT_EVALUATED")
        thr = item.get("threshold", {})
        if item["status"] != "EVALUATED":
            if item["blocking"]:
                pending_blocking.append(item["id"])
            acceptance.append({
                "id": item["id"], "blocking": item["blocking"], "threshold": thr,
                "value": None, "status": "NOT_EVALUATED", "pass": None,
            })
            continue
        value = item["value"]
        ok = True
        if "eq" in thr:
            ok = value == thr["eq"]
        elif "gte" in thr:
            ok = value >= thr["gte"]
        elif thr.get("report_only"):
            ok = True
        item["pass"] = ok
        acceptance.append({
            "id": item["id"], "blocking": item["blocking"], "threshold": thr,
            "value": value, "status": "EVALUATED", "pass": ok,
        })
        if not ok and item["blocking"]:
            failed_blocking.append(item["id"])

    if STOP or missing or gold["errors"] or failed_blocking:
        verdict = "REJECTED"
    elif pending_blocking:
        verdict = "VALIDATED_PENDING_PARSE"
    else:
        verdict = "ACCEPTED"

    report = {
        "spec_id": spec["spec_id"], "spec_version": spec["version"],
        "samples_root": os.path.abspath(args.samples),
        "counts": {
            "planned": spec["sample_plan"]["total_required_files"],
            "submitted": len(submitted),
            "unmapped": len(unmapped),
            "cases_ok": sum(1 for c in coverage if c["ok"]),
            "cases_total": len(coverage),
        },
        "coverage": coverage,
        "missing": missing,
        "unmapped_files": unmapped[:20],
        "gold": gold,
        "metrics": metrics,
        "acceptance": acceptance,
        "blocking_failures": failed_blocking,
        "blocking_pending": pending_blocking,
        "stop_conditions": STOP,
        "verdict": verdict,
    }

    out_path = os.path.join(os.path.dirname(os.path.abspath(args.samples)), "report.json")
    try:
        with open(out_path, "w", encoding="utf-8") as f:
            json.dump(report, f, ensure_ascii=False, indent=2)
    except OSError:
        out_path = None

    if args.json:
        print(json.dumps(report, ensure_ascii=False, indent=2))
    else:
        print("verdict: " + verdict)
        print("submitted/planned: " + str(report["counts"]["submitted"]) + "/" + str(report["counts"]["planned"]))
        print("cases ok: " + str(report["counts"]["cases_ok"]) + "/" + str(report["counts"]["cases_total"]))
        if missing:
            print("missing:")
            for m in missing:
                print("  [" + m["batch"] + "] " + m["code"] + " need " + str(m["need"]) + " have " + str(m["have"]) + " owner " + m["owner"])
        if unmapped:
            print("unmapped files: " + str(len(unmapped)) + " (e.g. " + ", ".join(unmapped[:3]) + ")")
        for err in gold["errors"][:10]:
            print("gold error: " + err)
        for s in STOP:
            print("stop: " + s)
        if out_path:
            print("report: " + out_path)
    sys.exit(0 if verdict in ("ACCEPTED", "VALIDATED_PENDING_PARSE") else 1)


if __name__ == "__main__":
    main()
