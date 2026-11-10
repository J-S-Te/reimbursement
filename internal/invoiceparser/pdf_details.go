package invoiceparser

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

type detailColumn struct {
	kind string
	x    float64
}

// detailColumns 识别同一行具有名称及金额的明细表头坐标。
// 参数：line为单页文字行。
// 返回值：从左到右的明确列；没有足够表头时返回空。
func detailColumns(line textLine) []detailColumn {
	aliases := map[string]string{"项目名称": "name", "货物或应税劳务、服务名称": "name", "货物或应税劳务名称": "name", "规格型号": "model", "单位": "unit", "数量": "quantity", "单价": "unit_price", "金额": "amount", "税率": "tax_rate", "税率/征收率": "tax_rate", "税额": "tax"}
	columns := []detailColumn{}
	hasName, hasAmount := false, false
	for _, run := range line.Runs {
		kind, ok := aliases[compactText(run.Text)]
		if !ok {
			continue
		}
		columns = append(columns, detailColumn{kind: kind, x: run.Source.X})
		hasName = hasName || kind == "name"
		hasAmount = hasAmount || kind == "amount"
	}
	if !hasName || !hasAmount {
		return nil
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].x < columns[j].x })
	return columns
}

// detailCells 使用表头列中点划分行文字，避免单价被误当金额。
// 参数：line为明细文字行，columns为当前表头坐标。
// 返回值：按列累积文字及其来源。
func detailCells(line textLine, columns []detailColumn) (map[string]string, []Source) {
	values := map[string]string{}
	sources := []Source{}
	for _, run := range line.Runs {
		best, distance := -1, math.MaxFloat64
		for i, column := range columns {
			d := math.Abs(run.Source.X - column.x)
			if d < distance {
				best, distance = i, d
			}
		}
		if best < 0 {
			continue
		}
		kind := columns[best].kind
		if values[kind] != "" {
			values[kind] += " "
		}
		values[kind] += normalizeText(run.Text)
		source := run.Source
		source.Region = "detail"
		source.Field = "detail." + kind
		sources = append(sources, source)
	}
	return values, sources
}

// detailFromCells 构造保留原精度的明细，不对数量单价做未经规则授权的舍入。
// 参数：c为候选诊断容器，values为列文字，sources为真实坐标来源。
// 返回值：带精确整数分及精度问题的明细。
func detailFromCells(c *Candidate, values map[string]string, sources []Source) DetailLine {
	detail := DetailLine{Name: values["name"], Model: values["model"], Unit: values["unit"], Quantity: values["quantity"], UnitPrice: values["unit_price"], TaxRate: values["tax_rate"], Sources: sources, Issues: []string{}}
	for _, spec := range []struct {
		key    string
		target *string
	}{{"amount", &detail.AmountMinor}, {"tax", &detail.TaxMinor}} {
		raw := values[spec.key]
		if raw == "" {
			continue
		}
		amount, err := Minor(raw)
		if err != nil {
			detail.Issues = append(detail.Issues, "DETAIL_MONEY_INVALID:"+spec.key)
			addIssue(c, "DETAIL_AMOUNTS_REQUIRE_REVIEW")
			continue
		}
		*spec.target = strconv.FormatInt(amount, 10)
	}
	return detail
}

// extractDetailLines 仅处理可识别表头下的行，并以汇总标签停止明细解析。
// 参数：c为票据候选，lines为单页坐标文字行。
// 返回值：无；不完整行保留明细问题，页间关系仍由候选组人工确认。
func extractDetailLines(c *Candidate, lines []textLine) {
	var columns []detailColumn
	for _, line := range lines {
		if header := detailColumns(line); len(header) > 0 {
			columns = header
			continue
		}
		if len(columns) == 0 {
			continue
		}
		combined := ""
		for _, run := range line.Runs {
			combined += compactText(run.Text)
		}
		if strings.Contains(combined, "价税合计") || strings.Contains(combined, "不含税金额") || strings.Contains(combined, "税额合计") {
			columns = nil
			continue
		}
		values, sources := detailCells(line, columns)
		if strings.Trim(values["name"], " :：") == "合计" {
			for _, spec := range []struct{ column, field string }{{"amount", "amount_minor"}, {"tax", "tax_minor"}} {
				if values[spec.column] != "" {
					source := Source{Page: line.Page, Text: values[spec.column], Method: "PDF_TABLE_TOTAL", Region: "detail_total"}
					for _, s := range sources {
						if s.Field == "detail."+spec.column {
							source = s
							break
						}
					}
					addMoneyField(c, spec.field, values[spec.column], source)
				}
			}
			columns = nil
			continue
		}
		if values["name"] == "" {
			addIssue(c, "UNASSIGNED_DETAIL_ROW_REQUIRES_REVIEW")
			continue
		}
		detail := detailFromCells(c, values, sources)
		if detail.AmountMinor == "" {
			detail.Issues = append(detail.Issues, "INCOMPLETE_OR_WRAPPED_DETAIL_REQUIRES_REVIEW")
		}
		c.DetailLines = append(c.DetailLines, detail)
	}
}
