package invoiceparser

import (
	"encoding/json"
	"errors"
	"slices"
)

const hardMaxOutputBytes = 4 << 20

// boundResultEvidence 用途：使解析结果与父进程输出上限一致，避免冗余全文挤掉有效候选。
// 参数：result 为待输出的完整解析结果，候选、明细及其字段来源不得删减。
// 返回值：可直接编码的人工复核结果；不可约证据超限时明确失败而不截断候选。
func boundResultEvidence(result Result) (Result, error) {
	if fits, err := resultFitsOutput(result); err != nil || fits {
		return result, err
	}
	for i := range result.Documents {
		compactCandidateEvidence(&result.Documents[i])
	}
	result.Warnings = append(result.Warnings, "EVIDENCE_COMPACTED_REFER_TO_ORIGINAL")
	fits, err := resultFitsOutput(result)
	if err != nil {
		return Result{}, err
	}
	if !fits {
		return Result{}, errors.New("PARSER_OUTPUT_LIMIT")
	}
	return result, nil
}

// resultFitsOutput 用途：连同命令信封和换行检查实际JSON输出长度。
// 参数：result 为包含全部候选证据的解析结果。
// 返回值：是否满足统一4MiB上限；不可编码的结果返回稳定协议错误。
func resultFitsOutput(result Result) (bool, error) {
	raw, err := json.Marshal(struct {
		Result Result `json:"result"`
		Code   string `json:"code"`
	}{Result: result})
	if err != nil {
		return false, errors.New("PARSER_RESULT_INVALID")
	}
	return len(raw)+1 <= hardMaxOutputBytes, nil
}

// compactCandidateEvidence 用途：移除重复全文副本，仍保留全部字段、明细证据和每页定位锚点。
// 参数：c 为需缩小输出的候选，原件仍是完整文字和结构化XML的权威来源。
// 返回值：无；Raw记录原证据数量与范围，Issues明确提示回看原件。
func compactCandidateEvidence(c *Candidate) {
	originalCount, originalRawBytes := len(c.Sources), len(c.Raw)
	retained := candidateEvidenceSources(c)
	c.Raw, _ = json.Marshal(map[string]any{
		"evidence_compacted":    true,
		"evidence_scope":        "ALL_FIELD_DETAIL_SOURCES_AND_PAGE_ANCHORS",
		"original_source_count": originalCount,
		"original_raw_bytes":    originalRawBytes,
		"retained_source_count": len(retained),
	})
	c.Sources = retained
	addIssue(c, "EVIDENCE_COMPACTED_REFER_TO_ORIGINAL")
}

// candidateEvidenceSources 用途：按稳定顺序收集完整候选与明细来源，并保留页级原件入口。
// 参数：c 为尚包含全文来源和全部字段候选的文档候选。
// 返回值：仅去除完全相同来源的证据数组，不修改字段候选内部来源。
func candidateEvidenceSources(c *Candidate) []Source {
	all := []Source{}
	pages := map[int]bool{}
	for _, source := range c.Sources {
		if !pages[source.Page] {
			all = append(all, source)
			pages[source.Page] = true
		}
	}
	fields := make([]string, 0, len(c.FieldCandidates))
	for field := range c.FieldCandidates {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	for _, field := range fields {
		for _, value := range c.FieldCandidates[field] {
			all = append(all, value.Sources...)
		}
	}
	for _, line := range c.DetailLines {
		all = append(all, line.Sources...)
	}
	seen := map[Source]bool{}
	retained := []Source{}
	for _, source := range all {
		if !seen[source] {
			seen[source] = true
			retained = append(retained, source)
		}
	}
	return retained
}
