package invoiceparser

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

var numberPattern = regexp.MustCompile(`[0-9]+`)
var codePattern = regexp.MustCompile(`[0-9]+`)
var datePattern = regexp.MustCompile(`[0-9]{4}(?:年[0-9]{1,2}月[0-9]{1,2}日|[-/][0-9]{1,2}[-/][0-9]{1,2})`)
var taxIDPattern = regexp.MustCompile(`^[A-Z0-9]{15,20}$`)
var moneyPattern = regexp.MustCompile(`[-−]?[￥¥]?[０-９0-9][０-９0-9,，.．eE+−-]*`)

type partyRegion struct {
	kind                     string
	anchorX                  float64
	left, right, top, bottom float64
}

// labelValue 读取明确标签同行的值，必要时仅使用紧邻右侧文字区域。
// 参数：runs为同一文字行，index为标签所在区域，labels为业务标签集合。
// 返回值：标签后的文字和对应来源；没有值时返回空字符串。
func labelValue(runs []textRun, index int, labels ...string) (string, Source) {
	run := runs[index]
	text := compactText(run.Text)
	for _, label := range labels {
		at := strings.Index(text, label)
		if at < 0 {
			continue
		}
		value := strings.TrimLeft(text[at+len(label):], ":：")
		if value != "" {
			return value, run.Source
		}
		if index+1 < len(runs) {
			next := runs[index+1]
			gap := next.Source.X - (run.Source.X + run.Source.Width)
			if gap >= -2 && gap <= 80 {
				return compactText(next.Text), next.Source
			}
		}
	}
	return "", run.Source
}

// extractHeaderFields 通过明确票号日期标签收集所有候选，不按页面推断独立发票。
// 参数：c为文件候选，lines为一页的坐标文字行。
// 返回值：无；票号日期冲突在统一解析阶段保留。
func extractHeaderFields(c *Candidate, lines []textLine) {
	for _, line := range lines {
		for i, run := range line.Runs {
			text := compactText(run.Text)
			if !strings.ContainsAny(text, "0123456789:") && strings.Contains(text, "发票") && (strings.Contains(text, "专用") || strings.Contains(text, "普通") || strings.Contains(text, "电子")) {
				addField(c, "invoice_type", text, run.Text, run.Source)
			}
			for _, spec := range []struct {
				field   string
				labels  []string
				pattern *regexp.Regexp
			}{
				{"invoice_number", []string{"发票号码", "发票号", "票据号码"}, numberPattern},
				{"invoice_code", []string{"发票代码"}, codePattern},
				{"invoice_date", []string{"开票日期", "出票日期"}, datePattern},
			} {
				value, source := labelValue(line.Runs, i, spec.labels...)
				if value == "" {
					continue
				}
				matches := spec.pattern.FindAllString(value, -1)
				for _, match := range matches {
					if spec.field == "invoice_number" && (len(match) < 8 || len(match) > 20) {
						addIssue(c, "INVOICE_NUMBER_FORMAT_REQUIRES_REVIEW")
						continue
					}
					if spec.field == "invoice_code" && (len(match) < 10 || len(match) > 12) {
						addIssue(c, "INVOICE_CODE_FORMAT_REQUIRES_REVIEW")
						continue
					}
					normalized := match
					if spec.field == "invoice_date" {
						normalized = canonicalDate(match)
					}
					addField(c, spec.field, normalized, match, source)
				}
			}
		}
	}
}

// partyRegions 由购销方锚点划定保守区域，缺少锚点时不猜测身份归属。
// 参数：page为页面几何，lines为该页文字行。
// 返回值：有明确边界的购销方区域集合。
func partyRegions(page layoutPage, lines []textLine) []partyRegion {
	regions := []partyRegion{}
	for _, line := range lines {
		for _, run := range line.Runs {
			text := compactText(run.Text)
			for _, party := range []struct{ label, kind string }{{"购买方", "buyer"}, {"购方", "buyer"}, {"销售方", "seller"}, {"销方", "seller"}} {
				if text == party.label || strings.HasPrefix(text, party.label+"信息") {
					regions = append(regions, partyRegion{kind: party.kind, anchorX: run.Source.X, left: 0, right: page.Width, top: line.Top - 3, bottom: line.Top + 160})
					break
				}
			}
		}
	}
	regions = append(regions, verticalPartyRegions(page)...)
	if len(regions) > 16 {
		return nil
	}
	for i := range regions {
		for j := range regions {
			if i == j || regions[i].kind == regions[j].kind {
				continue
			}
			if math.Abs(regions[i].top-regions[j].top) <= 20 {
				// 同行左右区域以锚点中点分界，避免名称和税号跨栏归属。
				leftX, rightX := regions[i].anchorX, regions[j].anchorX
				mid := (leftX + rightX) / 2
				if leftX < rightX {
					regions[i].right = mid
				} else {
					regions[i].left = mid
				}
			} else if regions[j].top > regions[i].top {
				regions[i].bottom = math.Min(regions[i].bottom, regions[j].top)
			}
		}
	}
	for i := range regions {
		for _, line := range lines {
			if line.Top <= regions[i].top {
				continue
			}
			for _, run := range line.Runs {
				text := compactText(run.Text)
				if strings.Contains(text, "项目名称") || strings.Contains(text, "货物或应税劳务") {
					regions[i].bottom = math.Min(regions[i].bottom, line.Top)
				}
			}
		}
	}
	return regions
}

// verticalPartyRegions 识别连续竖排购销方字形，避免把无区域通用税号猜作购销方。
// 参数：page为原始字形页，坐标按PDF左下原点解释。
// 返回值：仅由完整三字锚点支持的区域；字距或顺序不符不生成区域。
func verticalPartyRegions(page layoutPage) []partyRegion {
	columns := map[int][]Source{}
	for _, glyph := range page.Glyphs {
		text := compactText(glyph.Text)
		if text != "购" && text != "买" && text != "销" && text != "售" && text != "方" {
			continue
		}
		key := int(math.Round(glyph.X / 3))
		columns[key] = append(columns[key], glyph)
	}
	regions := []partyRegion{}
	for _, glyphs := range columns {
		sort.SliceStable(glyphs, func(i, j int) bool { return glyphs[i].Y > glyphs[j].Y })
		for i := 0; i+2 < len(glyphs); i++ {
			first, second, third := glyphs[i], glyphs[i+1], glyphs[i+2]
			word := compactText(first.Text + second.Text + third.Text)
			kind := ""
			if word == "购买方" {
				kind = "buyer"
			}
			if word == "销售方" {
				kind = "seller"
			}
			if kind == "" {
				continue
			}
			maximum := math.Max(24, math.Abs(first.Height)*3)
			if first.Y-second.Y <= 0 || first.Y-second.Y > maximum || second.Y-third.Y <= 0 || second.Y-third.Y > maximum {
				continue
			}
			top := page.Height - first.Y
			regions = append(regions, partyRegion{kind: kind, anchorX: first.X, left: 0, right: page.Width, top: top - 3, bottom: top + 160})
		}
	}
	return regions
}

// nextPartyTaxID 解析“标签单独一行、税号在下一行同栏”的常见排版。
// 只在同一购销方区域、紧邻行、同一起始横坐标范围内取符合税号格式的值，
// 不把项目明细或其他栏位的号码猜作购销方税号。
func nextPartyTaxID(line textLine, runIndex int, lines []textLine, regions []partyRegion, kind string) (string, Source) {
	label := line.Runs[runIndex]
	for _, next := range lines {
		if next.Page != line.Page || next.Top <= line.Top || next.Top-line.Top > 40 {
			continue
		}
		inRegion := false
		for _, region := range regions {
			if region.kind == kind && next.Top >= region.top && next.Top < region.bottom {
				inRegion = true
				break
			}
		}
		if !inRegion {
			continue
		}
		for _, run := range next.Runs {
			value := compactText(run.Text)
			if run.Source.X < label.Source.X-5 || run.Source.X > label.Source.X+label.Source.Width+100 {
				continue
			}
			if taxIDPattern.MatchString(value) {
				return value, run.Source
			}
		}
	}
	return "", Source{}
}

// extractPartyFields 仅在唯一购销方区域内解释通用名称和纳税人识别号标签。
// 参数：c为文件候选，page为页面几何，lines为坐标文字行。
// 返回值：无；不明确区域作为问题保留，不把通用税号强行归入某方。
func extractPartyFields(c *Candidate, page layoutPage, lines []textLine) {
	regions := partyRegions(page, lines)
	if len(regions) == 0 {
		addIssue(c, "PARTY_REGION_UNKNOWN")
	}
	for _, line := range lines {
		for i, run := range line.Runs {
			kinds := map[string]bool{}
			for _, region := range regions {
				if line.Top >= region.top && line.Top < region.bottom && run.Source.X >= region.left && run.Source.X < region.right {
					kinds[region.kind] = true
				}
			}
			for _, kind := range []string{"buyer", "seller"} {
				prefix := "购买方"
				if kind == "seller" {
					prefix = "销售方"
				}
				for _, field := range []struct {
					suffix string
					labels []string
				}{{"name", []string{"名称"}}, {"tax_id", []string{"统一社会信用代码/纳税人识别号", "纳税人识别号", "统一社会信用代码"}}} {
					explicit := []string{}
					for _, label := range field.labels {
						explicit = append(explicit, prefix+label)
					}
					value, source := labelValue(line.Runs, i, explicit...)
					if value == "" && len(kinds) == 1 && kinds[kind] {
						value, source = labelValue(line.Runs, i, field.labels...)
					}
					if value == "" && field.suffix == "tax_id" && len(kinds) == 1 && kinds[kind] {
						value, source = nextPartyTaxID(line, i, lines, regions, kind)
					}
					if value == "" {
						continue
					}
					if field.suffix == "tax_id" {
						value = strings.ToUpper(value)
						if !taxIDPattern.MatchString(value) {
							addIssue(c, "PARTY_TAX_ID_FORMAT_REQUIRES_REVIEW")
							continue
						}
					}
					source.Region = kind
					addField(c, kind+"_"+field.suffix, value, value, source)
				}
			}
		}
	}
}

// extractTotals 仅从明确金额标签解释汇总，不从任意数字猜测价税合计。
// 参数：c为文件候选，lines为本页文字行。
// 返回值：无；重复不同金额保留为候选冲突。
func extractTotals(c *Candidate, lines []textLine) {
	for _, line := range lines {
		for i := range line.Runs {
			for _, spec := range []struct {
				field  string
				labels []string
			}{
				{"amount_minor", []string{"不含税金额", "合计金额", "金额合计"}},
				{"tax_minor", []string{"税额合计", "合计税额"}},
				{"gross_minor", []string{"价税合计(小写)", "价税合计（小写）", "价税合计", "小写"}},
			} {
				value, source := labelValue(line.Runs, i, spec.labels...)
				if value == "" {
					continue
				}
				for _, amount := range moneyPattern.FindAllString(value, -1) {
					addMoneyField(c, spec.field, amount, source)
				}
			}
		}
	}
}
