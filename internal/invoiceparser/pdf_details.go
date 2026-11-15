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

// detailColumns 合并分字表头并识别名称与金额列的中心坐标。
// 参数：line为单页文字行。
// 返回值：从左到右的明确列；没有足够表头时返回空。
func detailColumns(line textLine) []detailColumn {
	aliases := map[string]string{"项目名称": "name", "货物或应税劳务、服务名称": "name", "货物或应税劳务名称": "name", "规格型号": "model", "单位": "unit", "数量": "quantity", "单价": "unit_price", "金额": "amount", "税率": "tax_rate", "税率/征收率": "tax_rate", "税额": "tax"}
	columns := []detailColumn{}
	hasName, hasAmount := false, false
	for i := 0; i < len(line.Runs); i++ {
		first, text := line.Runs[i], ""
		for j := i; j < len(line.Runs) && j < i+4; j++ {
			run := line.Runs[j]
			if j > i {
				previous := line.Runs[j-1].Source
				if run.Source.X-previous.X-previous.Width > math.Max(20, run.Source.Height*2) {
					break
				}
			}
			text += compactText(run.Text)
			kind, ok := aliases[text]
			if !ok {
				continue
			}
			center := (first.Source.X + run.Source.X + run.Source.Width) / 2
			columns = append(columns, detailColumn{kind: kind, x: center})
			hasName = hasName || kind == "name"
			hasAmount = hasAmount || kind == "amount"
			i = j
			break
		}
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
			d := math.Abs(run.Source.X + run.Source.Width/2 - column.x)
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
	previousTop := 0.0
	for _, line := range lines {
		if header := detailColumns(line); len(header) > 0 {
			columns = header
			previousTop = 0
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
		if compactText(values["name"]+values["model"]) == "合计" {
			addTableTotals(c, values, sources)
			columns = nil
			continue
		}
		if previousTop > 0 && appendWrappedDetail(c, values, sources, line.Top-previousTop) {
			previousTop = line.Top
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
		previousTop = line.Top
	}
}

// addTableTotals 将明确“合计”行中的金额与税额按列保存为汇总候选。
// 参数：c 为票据候选，values 为列文字，sources 为该行实际坐标来源。
// 返回值：无；缺失列保持缺失，冲突仍由统一候选规则处理。
func addTableTotals(c *Candidate, values map[string]string, sources []Source) {
	for _, spec := range []struct{ column, field string }{
		{"amount", "amount_minor"}, {"tax", "tax_minor"},
	} {
		if values[spec.column] == "" {
			continue
		}
		for _, source := range sources {
			if source.Field == "detail."+spec.column {
				addMoneyField(c, spec.field, values[spec.column], source)
				break
			}
		}
	}
}

// appendWrappedDetail 仅将紧邻且能闭合上一项括号的名称续行接回原明细。
// 参数：c 为已读候选，values/sources 为当前行，gap 为与上一行的垂直距离。
// 返回值：确定为续行时返回真；含数字列、跨段或无续写证据时保持独立明细。
func appendWrappedDetail(c *Candidate, values map[string]string, sources []Source, gap float64) bool {
	if len(c.DetailLines) == 0 || gap <= 0 || gap > 20 || values["name"] == "" {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(values["name"]), "*") {
		return false
	}
	for kind, value := range values {
		if kind != "name" && kind != "model" && value != "" {
			return false
		}
	}
	previous := &c.DetailLines[len(c.DetailLines)-1]
	if previous.AmountMinor == "" ||
		strings.Count(previous.Name, "(") <= strings.Count(previous.Name, ")") {
		return false
	}
	joinedName := previous.Name + values["name"]
	if strings.Count(joinedName, "(") != strings.Count(joinedName, ")") {
		return false
	}
	previous.Name = joinedName
	previous.Model += values["model"]
	previous.Sources = append(previous.Sources, sources...)
	return true
}
