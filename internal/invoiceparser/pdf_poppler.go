package invoiceparser

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// pdfPopplerMethod 标记来自 pdftotext 词框的坐标来源，便于复核时区分锁定库字形。
const pdfPopplerMethod = "PDF_POPPLER_WORD_BOX"

// pdfPopplerMaxOutput 限制 pdftotext 的 XHTML 输出，避免异常 PDF 放大进程内存。
const pdfPopplerMaxOutput = 8 << 20

// limitedWriter 在达到上限后拒绝继续写入。
type limitedWriter struct {
	buffer bytes.Buffer
	max    int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if writer.buffer.Len()+len(data) > writer.max {
		return 0, errors.New("PDF_POPPLER_OUTPUT_LIMIT")
	}
	return writer.buffer.Write(data)
}

// popplerBinary 解析 pdftotext 的固定可执行文件。
// 解析子进程使用最小环境（不继承 PATH），因此先按 PATH 查找，再回退到镜像内的
// 固定安装位置；不允许从业务 cwd 或环境变量扩张执行路径。
// 参数：无。
// 返回值：可执行的绝对路径或缺失错误。
func popplerBinary() (string, error) {
	if path, err := exec.LookPath("pdftotext"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/usr/bin/pdftotext", "/usr/local/bin/pdftotext"} {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", errors.New("PDF_POPPLER_UNAVAILABLE")
}

// pdfPagesHaveUsableText 判定锁定库提取结果是否值得直接进入字段适配。
// 全角/汉字丢失、替换符或控制字符过多时必须尝试具备 ToUnicode 能力的后备提取器；
// 纯英文文本不是本子系统的票面输入，不需要为它额外启动外部进程。
func pdfPagesHaveUsableText(pages []layoutPage) bool {
	runes, invalid, han := 0, 0, 0
	for _, page := range pages {
		for _, glyph := range page.Glyphs {
			text := strings.TrimSpace(glyph.Text)
			if text == "" {
				continue
			}
			for _, r := range text {
				runes++
				if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cs, r) {
					invalid++
				}
				if unicode.Is(unicode.Han, r) {
					han++
				}
			}
		}
	}
	if runes < 10 || han == 0 {
		return false
	}
	return invalid*20 <= runes
}

// popplerPDFPages 通过 pdftotext 的 bbox-layout XHTML 读取词框。
// 仅作为锁定库无法还原 ToUnicode 时的后备路径；二进制缺失、超时或输出异常都返回错误，
// 调用方保留锁定库的原结果，不能把后备失败伪装成可解析。
func popplerPDFPages(ctx context.Context, data []byte) ([]layoutPage, error) {
	path, err := popplerBinary()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, "-bbox-layout", "-enc", "UTF-8", "-", "-")
	command.Stdin = bytes.NewReader(data)
	output := &limitedWriter{max: pdfPopplerMaxOutput}
	command.Stdout = output
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	if err = command.Run(); err != nil {
		return nil, fmt.Errorf("PDF_POPPLER_FAILED: %w", err)
	}
	return parsePopplerLayout(output.buffer.Bytes())
}

// popplerWord 是 XHTML 中一个词的几何与文本。
type popplerWord struct {
	xMin, yMin, xMax, yMax float64
	text                   strings.Builder
}

// parsePopplerLayout 解析 pdftotext -bbox-layout 输出，保留页尺寸和每个词的词框。
// 参数：data 为受上限约束的 XHTML。
// 返回值：按页组织的词框；缺少页面尺寸或词框时明确失败。
func parsePopplerLayout(data []byte) ([]layoutPage, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false
	pages := []layoutPage{}
	var current *layoutPage
	var word *popplerWord
	var width, height float64
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("PDF_POPPLER_LAYOUT_INVALID")
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "page":
				width, height = popplerFloat(value, "width"), popplerFloat(value, "height")
				if width <= 0 || height <= 0 || math.IsNaN(width) || math.IsNaN(height) {
					return nil, errors.New("PDF_POPPLER_PAGE_GEOMETRY_INVALID")
				}
				current = &layoutPage{Number: len(pages) + 1, Width: width, Height: height}
			case "word":
				if current == nil {
					return nil, errors.New("PDF_POPPLER_WORD_OUTSIDE_PAGE")
				}
				word = &popplerWord{
					xMin: popplerFloat(value, "xMin"), yMin: popplerFloat(value, "yMin"),
					xMax: popplerFloat(value, "xMax"), yMax: popplerFloat(value, "yMax"),
				}
			}
		case xml.CharData:
			if word != nil {
				word.text.Write([]byte(value))
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "word":
				if word == nil || current == nil {
					continue
				}
				text := strings.TrimSpace(word.text.String())
				if text != "" {
					if word.xMax < word.xMin || word.yMax < word.yMin {
						return nil, errors.New("PDF_POPPLER_WORD_GEOMETRY_INVALID")
					}
					current.Glyphs = append(current.Glyphs, splitPopplerWord(word, current.Height, current.Number)...)
				}
				word = nil
			case "page":
				if current == nil || len(current.Glyphs) == 0 {
					return nil, errors.New("PDF_POPPLER_NO_TEXT")
				}
				pages = append(pages, *current)
				current = nil
			}
		}
	}
	if len(pages) == 0 {
		return nil, errors.New("PDF_POPPLER_NO_TEXT")
	}
	for _, page := range pages {
		if len(page.Glyphs) > 100000 {
			return nil, errors.New("PDF_POPPLER_TOO_MANY_WORDS")
		}
	}
	return pages, nil
}

// splitPopplerWord 将 pdftotext 合并的整行词按空白拆成独立来源。
// 票据合计常被输出为「合计金额：100.00 税额合计：13.00 价税合计（小写）：113.00」
// 一个 word；若整体交给标签解析，三个金额会互相污染。拆分时按字宽近似分摊原词框，
// 只影响来源坐标的精度，不改变文本和金额值。
func splitPopplerWord(word *popplerWord, pageHeight float64, page int) []Source {
	text := strings.TrimSpace(word.text.String())
	parts := strings.Fields(text)
	if len(parts) <= 1 {
		return []Source{{Page: page, X: word.xMin, Y: pageHeight - word.yMax,
			Width: word.xMax - word.xMin, Height: word.yMax - word.yMin,
			Text: text, Method: pdfPopplerMethod}}
	}
	totalWeight := 0.0
	weights := make([]float64, len(parts))
	for i, part := range parts {
		for _, r := range part {
			weights[i] += 1
			if unicode.Is(unicode.Han, r) {
				weights[i] += 1
			}
		}
		totalWeight += weights[i]
	}
	if totalWeight <= 0 {
		totalWeight = float64(len(parts))
		for i := range weights {
			weights[i] = 1
		}
	}
	sources := make([]Source, 0, len(parts))
	offset := word.xMin
	width := word.xMax - word.xMin
	// 留出明显的词间距，避免 rebuildTextLines 把拆分后的“金额/税额/价税合计”
	// 又合并回同一个 run；间距只影响来源框，不改变文本顺序。
	gap := 0.0
	if len(parts) > 1 {
		gap = width * 0.4 / float64(len(parts)-1)
	}
	usable := width - gap*float64(len(parts)-1)
	for i, part := range parts {
		partWidth := usable * weights[i] / totalWeight
		sources = append(sources, Source{Page: page, X: offset, Y: pageHeight - word.yMax,
			Width: partWidth, Height: word.yMax - word.yMin, Text: part, Method: pdfPopplerMethod})
		offset += partWidth + gap
	}
	return sources
}

// popplerFloat 读取固定的浮点属性，缺失或非法时返回 0。
func popplerFloat(element xml.StartElement, name string) float64 {
	for _, attribute := range element.Attr {
		if attribute.Name.Local != name {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(attribute.Value), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0
		}
		return value
	}
	return 0
}
