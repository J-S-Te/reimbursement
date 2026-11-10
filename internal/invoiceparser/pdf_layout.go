package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
)

type layoutPage struct {
	Number        int
	Width, Height float64
	Rotation      int
	Glyphs        []Source
}
type textRun struct {
	Text   string `json:"text"`
	Source Source `json:"source"`
}
type textLine struct {
	Page int       `json:"page"`
	Top  float64   `json:"top"`
	Runs []textRun `json:"runs"`
}

// inheritedPageValue 有界读取PDF页面继承属性，不为缺失几何信息编造默认纸张。
// 参数：page为页面字典，key为固定属性名。
// 返回值：属性值或空Value；循环和过深继承在64层停止。
func inheritedPageValue(page pdf.Value, key string) pdf.Value {
	for depth := 0; depth < 64 && !page.IsNull(); depth++ {
		value := page.Key(key)
		if !value.IsNull() {
			return value
		}
		page = page.Key("Parent")
	}
	return pdf.Value{}
}

// parsePDF 读取锁定PDF库的字形、页面尺寸及旋转，再运行保守布局适配。
// 参数：ctx为取消边界，data为受大小限制的PDF原件。
// 返回值：含来源和人工复核状态的候选组；加密、无文字或超限明确失败。
func (p *Parser) parsePDF(ctx context.Context, data []byte) (Result, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, errors.New("INVALID_OR_ENCRYPTED_PDF")
	}
	count := reader.NumPage()
	if count < 1 {
		return Result{}, errors.New("INVALID_OR_ENCRYPTED_PDF")
	}
	if count > p.limits.MaxPages {
		return Result{}, errors.New("PDF_PAGE_LIMIT")
	}
	pages := make([]layoutPage, 0, count)
	glyphs, textBytes := 0, 0
	for number := 1; number <= count; number++ {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		page := reader.Page(number)
		if page.V.IsNull() {
			return Result{}, errors.New("INVALID_PDF_PAGE")
		}
		box := inheritedPageValue(page.V, "MediaBox")
		width, height := 0.0, 0.0
		if box.Len() == 4 {
			width = box.Index(2).Float64() - box.Index(0).Float64()
			height = box.Index(3).Float64() - box.Index(1).Float64()
		}
		if math.IsNaN(width) || math.IsNaN(height) || math.IsInf(width, 0) || math.IsInf(height, 0) {
			return Result{}, errors.New("INVALID_PDF_GEOMETRY")
		}
		current := layoutPage{Number: number, Width: width, Height: height, Rotation: int(inheritedPageValue(page.V, "Rotate").Int64())}
		content := page.Content()
		glyphs += len(content.Text)
		if glyphs > 100000 {
			return Result{}, errors.New("PDF_TEXT_LIMIT")
		}
		for _, glyph := range content.Text {
			textBytes += len(glyph.S)
			if textBytes > 1<<20 {
				return Result{}, errors.New("PDF_TEXT_LIMIT")
			}
			if math.IsNaN(glyph.X) || math.IsNaN(glyph.Y) || math.IsInf(glyph.X, 0) || math.IsInf(glyph.Y, 0) || math.IsInf(glyph.W, 0) || math.IsNaN(glyph.W) || math.IsNaN(glyph.FontSize) || math.IsInf(glyph.FontSize, 0) {
				return Result{}, errors.New("INVALID_PDF_GEOMETRY")
			}
			current.Glyphs = append(current.Glyphs, Source{Page: number, X: glyph.X, Y: glyph.Y, Width: glyph.W, Height: glyph.FontSize, Text: glyph.S, Method: "PDF_GLYPH_COORDINATES"})
		}
		pages = append(pages, current)
	}
	return adaptPDFPages(ctx, pages)
}

// rebuildTextLines 按原始PDF用户空间坐标重建文字行及相隔较远的字段区域。
// 参数：page为页面和字形；坐标保留PDF左下原点，Top只用于逻辑排序。
// 返回值：从上到下、同行从左到右的行；原字形不会跨大空隙直接粘连。
func rebuildTextLines(page layoutPage) []textLine {
	glyphs := append([]Source{}, page.Glyphs...)
	sort.SliceStable(glyphs, func(i, j int) bool {
		if glyphs[i].Y != glyphs[j].Y {
			return glyphs[i].Y > glyphs[j].Y
		}
		return glyphs[i].X < glyphs[j].X
	})
	var groups [][]Source
	for _, glyph := range glyphs {
		if strings.TrimSpace(glyph.Text) == "" {
			continue
		}
		if len(groups) == 0 || math.Abs(groups[len(groups)-1][0].Y-glyph.Y) > math.Max(2, glyph.Height*.3) {
			groups = append(groups, []Source{glyph})
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], glyph)
		}
	}
	lines := make([]textLine, 0, len(groups))
	for _, group := range groups {
		sort.SliceStable(group, func(i, j int) bool { return group[i].X < group[j].X })
		line := textLine{Page: page.Number, Top: page.Height - group[0].Y, Runs: []textRun{}}
		for _, glyph := range group {
			text := normalizeText(glyph.Text)
			if text == "" {
				continue
			}
			n := len(line.Runs)
			if n == 0 || glyph.X-(line.Runs[n-1].Source.X+line.Runs[n-1].Source.Width) > math.Max(18, glyph.Height*2) {
				line.Runs = append(line.Runs, textRun{Text: text, Source: glyph})
				continue
			}
			run := &line.Runs[n-1]
			gap := glyph.X - (run.Source.X + run.Source.Width)
			if gap > math.Max(2, glyph.Height*.3) {
				run.Text += " "
			}
			run.Text += text
			right := math.Max(run.Source.X+run.Source.Width, glyph.X+glyph.Width)
			run.Source.Width = right - run.Source.X
			run.Source.Height = math.Max(run.Source.Height, glyph.Height)
			run.Source.Text = run.Text
		}
		lines = append(lines, line)
	}
	return lines
}

// adaptPDFPages 从坐标文字页提取候选及不确定分组，不把页面数量等同于发票数量。
// 参数：ctx为取消边界，pages为已限制数量的字形页。
// 返回值：唯一明确候选或需人工拆分组；无可用文字层返回稳定错误。
func adaptPDFPages(ctx context.Context, pages []layoutPage) (Result, error) {
	c := newCandidate()
	allLines := []textLine{}
	runes := 0
	for _, page := range pages {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		c.Pages = append(c.Pages, PageInfo{Number: page.Number, Width: page.Width, Height: page.Height, Rotation: page.Rotation, GlyphCount: len(page.Glyphs), CoordinateSystem: "PDF_USER_SPACE_BOTTOM_LEFT"})
		c.SourcePages = append(c.SourcePages, page.Number)
		if page.Width <= 0 || page.Height <= 0 {
			addIssue(&c, "PAGE_GEOMETRY_UNKNOWN")
		}
		if page.Rotation%360 != 0 {
			addIssue(&c, "ROTATED_PAGE_REQUIRES_REVIEW")
		}
		for _, glyph := range page.Glyphs {
			runes += utf8.RuneCountInString(strings.TrimSpace(glyph.Text))
			if strings.ContainsRune(glyph.Text, utf8.RuneError) {
				addIssue(&c, "UNREADABLE_TEXT_ENCODING")
			}
			c.Sources = append(c.Sources, glyph)
		}
		lines := rebuildTextLines(page)
		allLines = append(allLines, lines...)
		extractHeaderFields(&c, lines)
		extractPartyFields(&c, page, lines)
		extractTotals(&c, lines)
		extractDetailLines(&c, lines)
	}
	if runes < 10 {
		return Result{}, errors.New("NO_USABLE_TEXT_LAYER")
	}
	resolveFields(&c)
	if len(c.FieldCandidates["invoice_number"]) > 1 {
		c.GroupKind = "AMBIGUOUS_MERGE_OR_CONTINUATION"
		addIssue(&c, "AMBIGUOUS_OR_MERGED_INVOICES")
	}
	if len(pages) > 1 {
		markPDFPageGrouping(&c)
	}
	c.Raw, _ = json.Marshal(struct {
		NormalizationVersion string     `json:"normalization_version"`
		Pages                []PageInfo `json:"pages"`
		Lines                []textLine `json:"lines"`
	}{normalizationVersion, c.Pages, allLines})
	return Result{Status: "NEEDS_REVIEW", ParserVersion: "pdf-5e46805f718e", RulesVersion: rulesVersion, NormalizationVersion: normalizationVersion, Documents: []Candidate{c}, Warnings: []string{"TEXT_EXTRACTION_IS_NOT_TAX_VERIFICATION"}}, nil
}

// markPDFPageGrouping 仅在每页重复同一票号时保留单一候选，否则要求人工识别合并或续页。
// 参数：c为完整文件候选与各字段来源。
// 返回值：无；不确定组通过GroupKind明确标记给业务适配器。
func markPDFPageGrouping(c *Candidate) {
	addIssue(c, "MULTIPAGE_DETAILS_REQUIRE_REVIEW")
	pages := map[int]bool{}
	if len(c.FieldCandidates["invoice_number"]) == 1 {
		for _, source := range c.FieldCandidates["invoice_number"][0].Sources {
			pages[source.Page] = true
		}
	}
	if len(pages) != len(c.Pages) || len(c.FieldCandidates["invoice_number"]) != 1 {
		c.GroupKind = "AMBIGUOUS_MERGE_OR_CONTINUATION"
		addIssue(c, "AMBIGUOUS_OR_MERGED_INVOICES")
	} else {
		c.GroupKind = "MULTIPAGE_SINGLE_CANDIDATE_REQUIRES_REVIEW"
	}
}
