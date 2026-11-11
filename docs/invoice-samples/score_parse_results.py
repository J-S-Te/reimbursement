#!/usr/bin/env python3
"""把解析结果与金标准比对，生成 validate_samples.py 可直接消费的 parse_report.json。

用法：
  python3 score_parse_results.py --parsed parsed.json --gold gold.csv --out parse_report.json
  python3 score_parse_results.py --parsed parsed.json --gold gold.csv --spec spec.v1.json

输入 parsed.json 支持两种形态：
  [{"file_name": "...", "status": "PARSED", "fields": {...}, "auto_confirmed": true, ...}]
  {"results": [ ... ]}

只做比对与归类，不修改输入、不推断缺失字段。
"""

import argparse
import csv
import json
import re
import sys

KEY_FIELDS = ["invoice_number", "seller_tax_id", "buyer_tax_id", "total_minor"]


def load(path, what):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except Exception as exc:  # noqa: BLE001
        print("ERROR: 无法读取" + what + ": " + str(exc), file=sys.stderr)
        sys.exit(2)


def norm(value):
    if value is None:
        return ""
    return str(value).strip()


def money_minor(value):
    text = norm(value).replace(",", "")
    if not text:
        return ""
    return text


def case_for(spec, file_name):
    m = re.match(spec["naming"]["pattern"], file_name)
    if not m:
        return None
    ticket, variant = m.group("ticket"), m.group("variant")
    ext = m.group("ext").lower()
    media = "ofd" if ext == "ofd" else ("image" if ext in ("jpg", "jpeg", "png") else "pdf")
    for batch in spec["sample_plan"]["batches"]:
        for case in batch["cases"]:
            if case["ticket"] != ticket or case["variant"] != variant:
                continue
            allowed = case["media"].split("|")
            if media == "pdf" and ("pdf_text" in allowed or "pdf_image" in allowed):
                manual = "pdf_image" in allowed and "pdf_text" not in allowed
                return batch["id"], case, manual
            if media in allowed:
                return batch["id"], case, media in ("image",) or "pdf_image" in allowed
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--spec", default="spec.v1.json")
    ap.add_argument("--gold", required=True)
    ap.add_argument("--parsed", required=True)
    ap.add_argument("--out", default="parse_report.json")
    args = ap.parse_args()

    spec = load(args.spec, "spec")
    gold = {}
    with open(args.gold, newline="", encoding="utf-8-sig") as f:
        for row in csv.DictReader(f):
            gold[norm(row.get("file_name"))] = row

    parsed = load(args.parsed, "parsed")
    items = parsed.get("results", parsed) if isinstance(parsed, dict) else parsed
    if not isinstance(items, list):
        print("ERROR: parsed 必须是数组或 {results: [...]}", file=sys.stderr)
        sys.exit(2)

    seen = set()
    results = []
    for item in items:
        name = norm(item.get("file_name"))
        if not name:
            continue
        seen.add(name)
        hit = case_for(spec, name)
        batch_id, case, manual_only = hit if hit else (None, None, False)
        g = gold.get(name)
        fields = item.get("fields") or {}
        diffs = []
        key_correct = None
        if g is not None and fields:
            key_correct = True
            for field in KEY_FIELDS:
                if norm(fields.get(field)) != money_minor(g.get(field)):
                    key_correct = False
                    diffs.append(field)
        status = norm(item.get("status")) or "UNKNOWN"
        crashed = bool(item.get("crashed"))
        auto_ok = status == "PARSED" and key_correct is True and not crashed
        results.append({
            "file_name": name,
            "case_code": (case or {}).get("code") or item.get("case_code") or "",
            "batch": batch_id or "",
            "parse_run_id": norm(item.get("parse_run_id")),
            "status": status,
            "auto_parsed_ok": auto_ok,
            "auto_confirmed": bool(item.get("auto_confirmed")),
            "key_fields_correct": key_correct,
            "manual_only_media": bool(manual_only),
            "manual_reviewed": bool(item.get("manual_reviewed")),
            "crashed": crashed,
            "silent_loss": bool(item.get("silent_loss")),
            "gold_diff": diffs,
            "notes": norm(item.get("notes")),
        })

    for name, g in gold.items():
        if name in seen:
            continue
        hit = case_for(spec, name)
        results.append({
            "file_name": name,
            "case_code": (hit[1] or {}).get("code") if hit else "",
            "batch": hit[0] if hit else "",
            "parse_run_id": "",
            "status": "MISSING",
            "auto_parsed_ok": False,
            "auto_confirmed": False,
            "key_fields_correct": None,
            "manual_only_media": bool(hit[2]) if hit else False,
            "manual_reviewed": False,
            "crashed": False,
            "silent_loss": False,
            "gold_diff": [],
            "notes": "解析结果缺失，不能计入成功",
        })

    report = {
        "schema_version": "1.0",
        "spec_id": spec["spec_id"],
        "spec_version": spec["version"],
        "results": sorted(results, key=lambda r: r["file_name"]),
    }
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(report, f, ensure_ascii=False, indent=2)

    auto = sum(1 for r in results if r["auto_parsed_ok"])
    mismatch = [r["file_name"] for r in results if r["key_fields_correct"] is False]
    missing = [r["file_name"] for r in results if r["status"] == "MISSING"]
    print("rows: " + str(len(results)) + " auto_ok: " + str(auto))
    print("key field mismatch: " + str(len(mismatch)))
    if mismatch:
        print("  " + ", ".join(mismatch[:5]))
    print("missing parse results: " + str(len(missing)))
    print("written: " + args.out)


if __name__ == "__main__":
    main()
