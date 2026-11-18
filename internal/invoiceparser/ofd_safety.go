package invoiceparser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"golang.org/x/text/encoding/simplifiedchinese"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
)

type xmlFact struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source Source `json:"source"`
}
type ofdInventory struct {
	DocumentRoots []string  `json:"document_roots"`
	InvoiceFiles  []string  `json:"invoice_files"`
	Facts         []xmlFact `json:"facts"`
	PageCount     int       `json:"page_count"`
	DocBodies     int       `json:"doc_bodies"`
	XMLFiles      []string  `json:"xml_files"`
	rootNames     map[string]string
}
type xmlNode struct {
	name, path  string
	text        strings.Builder
	children    map[string]int
	hasChildren bool
}

// safeArchivePath 校验ZIP条目及文档引用的规范路径，不允许等价路径或文件系统逃逸。
// 参数：name为原始ZIP内部路径。
// 返回值：不含目录结尾斜线的规范路径；非法时返回空。
func safeArchivePath(name string) string {
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.ContainsAny(trimmed, "\\:") || strings.HasPrefix(trimmed, "/") || path.Clean(trimmed) != trimmed {
		return ""
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return ""
		}
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "." || part == ".." || part == "" {
			return ""
		}
	}
	return trimmed
}

// checkOFD 保留原有安全预检入口并复用完整容器枚举。
// 参数：ctx为取消边界，data为有界OFD字节。
// 返回值：安全及格式预检错误，不执行字段业务提取。
func (p *Parser) checkOFD(ctx context.Context, data []byte) error {
	_, err := p.inspectOFD(ctx, data)
	return err
}

// inspectOFD 在第三方库读取前限制条目、实际展开量、XML深度与文档页数。
// 参数：ctx为取消边界，data为已限制原件大小的ZIP容器。
// 返回值：有界文档及字段来源索引；任何可疑结构均失败关闭。
func (p *Parser) inspectOFD(ctx context.Context, data []byte) (ofdInventory, error) {
	inventory := ofdInventory{rootNames: map[string]string{}}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return inventory, errors.New("INVALID_OFD")
	}
	if len(reader.File) > 2048 {
		return inventory, errors.New("OFD_ENTRY_LIMIT")
	}
	seen := map[string]bool{}
	expanded := uint64(0)
	tokens := 0
	for _, file := range reader.File {
		if ctx.Err() != nil {
			return inventory, ctx.Err()
		}
		name := safeArchivePath(file.Name)
		if name == "" || seen[name] {
			return inventory, errors.New("OFD_UNSAFE_PATH")
		}
		seen[name] = true
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 || mode.Type() != 0 && !mode.IsDir() {
			return inventory, errors.New("OFD_UNSAFE_FILE_TYPE")
		}
		if mode.IsDir() {
			continue
		}
		if file.UncompressedSize64 > p.limits.MaxExpandedBytes-expanded {
			return inventory, errors.New("OFD_EXPANDED_LIMIT")
		}
		isXML := strings.EqualFold(path.Ext(name), ".xml")
		maximum := p.limits.MaxExpandedBytes - expanded
		if isXML && maximum > 8<<20 {
			maximum = 8 << 20
		}
		stream, err := file.Open()
		if err != nil {
			return inventory, errors.New("INVALID_OFD")
		}
		var raw []byte
		var actual int64
		if isXML {
			raw, err = io.ReadAll(io.LimitReader(stream, int64(maximum)+1))
			actual = int64(len(raw))
		} else {
			actual, err = io.Copy(io.Discard, io.LimitReader(stream, int64(maximum)+1))
		}
		closeErr := stream.Close()
		if err != nil || closeErr != nil || uint64(actual) != file.UncompressedSize64 {
			return inventory, errors.New("INVALID_OFD")
		}
		if uint64(actual) > maximum {
			return inventory, errors.New("OFD_EXPANDED_OR_XML_LIMIT")
		}
		expanded += uint64(actual)
		if isXML {
			inventory.XMLFiles = append(inventory.XMLFiles, name)
			if err := scanOFDXML(ctx, name, raw, &inventory, &tokens); err != nil {
				return inventory, err
			}
		}
	}
	if !seen["OFD.xml"] || inventory.DocBodies == 0 || len(inventory.DocumentRoots) == 0 {
		return inventory, errors.New("INVALID_OFD")
	}
	for _, root := range inventory.DocumentRoots {
		if safeArchivePath(root) == "" || !seen[root] || inventory.rootNames[root] != "Document" {
			return inventory, errors.New("INVALID_OFD_DOCUMENT_REFERENCE")
		}
	}
	if inventory.PageCount > p.limits.MaxPages {
		return inventory, errors.New("OFD_PAGE_LIMIT")
	}
	return inventory, nil
}

// scanOFDXML 使用流式令牌预检，记录真实叶节点路径并拒绝DTD及实体声明。
// 参数：ctx为取消边界，name/raw为XML条目，inventory/tokens为文件级有界统计。
// 返回值：稳定XML错误；不加载外部实体也不执行处理指令。
func scanOFDXML(ctx context.Context, name string, raw []byte, inventory *ofdInventory, tokens *int) error {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.CharsetReader = ofdCharsetReader
	stack := []*xmlNode{}
	roots := 0
	recognized := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("INVALID_OFD_XML")
		}
		*tokens++
		if *tokens > 200000 {
			return errors.New("OFD_XML_TOKEN_LIMIT")
		}
		if *tokens%256 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		switch value := token.(type) {
		case xml.Directive:
			return errors.New("OFD_XML_DIRECTIVE_FORBIDDEN")
		case xml.ProcInst:
			if value.Target != "xml" {
				return errors.New("OFD_XML_INSTRUCTION_FORBIDDEN")
			}
		case xml.StartElement:
			if len(stack) >= 64 {
				return errors.New("OFD_XML_DEPTH_LIMIT")
			}
			parentPath := ""
			index := 1
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.hasChildren = true
				parent.children[value.Name.Local]++
				index = parent.children[value.Name.Local]
				parentPath = parent.path
			} else {
				roots++
				inventory.rootNames[name] = value.Name.Local
				if roots > 1 {
					return errors.New("INVALID_OFD_XML")
				}
				if name == "OFD.xml" && value.Name.Local != "OFD" {
					return errors.New("INVALID_OFD")
				}
			}
			node := &xmlNode{name: value.Name.Local, path: parentPath + "/" + value.Name.Local + "[" + strconv.Itoa(index) + "]", children: map[string]int{}}
			stack = append(stack, node)
			if name == "OFD.xml" && value.Name.Local == "DocBody" {
				inventory.DocBodies++
			}
			if value.Name.Local == "Page" && strings.Contains(parentPath, "/Pages[") {
				inventory.PageCount++
			}
		case xml.CharData:
			if len(stack) > 0 {
				node := stack[len(stack)-1]
				if node.text.Len()+len(value) > 1<<20 {
					return errors.New("OFD_XML_TEXT_LIMIT")
				}
				node.text.Write(value)
			} else if strings.TrimSpace(string(value)) != "" {
				return errors.New("INVALID_OFD_XML")
			}
		case xml.EndElement:
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			text := strings.TrimSpace(node.text.String())
			if name == "OFD.xml" && node.name == "DocRoot" {
				inventory.DocumentRoots = append(inventory.DocumentRoots, text)
			}
			if !node.hasChildren && text != "" && ofdFieldName(node.name, node.path) != "" {
				if len(inventory.Facts) >= 4096 {
					return errors.New("OFD_FIELD_LIMIT")
				}
				inventory.Facts = append(inventory.Facts, xmlFact{Name: node.name, Value: text, Source: Source{XMLPath: name + "#" + node.path, Text: text, Method: "OFD_XML_LEAF"}})
				if ofdFieldName(node.name, node.path) == "invoice_number" {
					recognized = true
				}
			}
		}
	}
	if roots != 1 || len(stack) != 0 {
		return errors.New("INVALID_OFD_XML")
	}
	if recognized {
		inventory.InvoiceFiles = append(inventory.InvoiceFiles, name)
	}
	return nil
}

// ofdFieldName 映射支持的结构化发票标签，购销方通用字段必须处于相应父节点。
// 参数：name为XML本地名称，xmlPath为带位置索引的路径。
// 返回值：统一候选字段名；不认识的标签返回空。
func ofdFieldName(name, xmlPath string) string {
	if strings.Contains(xmlPath, "/GoodsInfos[") {
		details := map[string]string{"Item": "detail.name", "Name": "detail.name", "Specification": "detail.model", "Model": "detail.model", "Unit": "detail.unit", "Quantity": "detail.quantity", "Price": "detail.unit_price", "Amount": "detail.amount", "TaxScheme": "detail.tax_rate", "TaxRate": "detail.tax_rate", "TaxAmount": "detail.tax"}
		return details[name]
	}
	if name == "Drawer" {
		return "issuer"
	}
	fields := map[string]string{"InvoiceNo": "invoice_number", "InvoiceNumber": "invoice_number", "ElectronicInvoiceRailwayETicketNumber": "invoice_number", "IssueDate": "invoice_date", "DateOfIssue": "invoice_date", "BuyerName": "buyer_name", "NameOfPurchaser": "buyer_name", "BuyerTaxID": "buyer_tax_id", "UnifiedSocialCreditCodeOfPurchaser": "buyer_tax_id", "SellerName": "seller_name", "IssueParty": "seller_name", "SellerTaxID": "seller_tax_id", "IssuePartyCode": "seller_tax_id", "TaxExclusiveTotalAmount": "amount_minor", "TotalAmountExcludingTax": "amount_minor", "TaxTotalAmount": "tax_minor", "TaxInclusiveTotalAmount": "gross_minor", "Fare": "gross_minor"}
	if name == "TaxAmount" && !strings.Contains(xmlPath, "/GoodsInfos[") {
		return "tax_minor"
	}
	if name == "Name" || name == "Code" {
		prefix := ""
		if strings.Contains(xmlPath, "/Buyer[") {
			prefix = "buyer_"
		}
		if strings.Contains(xmlPath, "/Seller[") {
			prefix = "seller_"
		}
		if prefix != "" {
			if name == "Name" {
				return prefix + "name"
			}
			return prefix + "tax_id"
		}
	}
	return fields[name]
}

// ofdCharsetReader 兼容锁定库的本地编码集合，未知编码失败关闭且不访问外部资源。
// 参数：charset为XML声明编码，input为已限制展开大小的字节流。
// 返回值：受预检令牌及文本限制的解码流；不支持的编码返回错误。
func ofdCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "gbk", "gb2312", "gb18030":
		return simplifiedchinese.GBK.NewDecoder().Reader(input), nil
	default:
		return nil, errors.New("OFD_XML_CHARSET_UNSUPPORTED")
	}
}
