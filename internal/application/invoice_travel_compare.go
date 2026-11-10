package application

import "github.com/j-s-te/reimbursement/internal/domain"

// crossCheckTravelReference 合并声明、当前项目和人工票面，未知不能被其他匹配覆盖。
// 参数：item为费用，ref为已验摘要的权威引用，base为原项目评估，checks为可信人工票面。
// 返回值：保留权威来源并附新交叉结果的逐项目结论。
func crossCheckTravelReference(item domain.Item, ref domain.ProjectReference, base domain.TravelAssessmentReference, checks []domain.TravelInvoiceCheck) domain.TravelAssessmentReference {
	projectStatus, projectReasons := compareDeclaredProjectTravel(item, ref, base)
	cross := &domain.TravelCrossCheck{PolicyVersion: "TRAVEL_DECLARATION_INVOICE_V1", Declaration: item.TravelDeclaration,
		Status: projectStatus, ProjectStatus: projectStatus, ProjectReasonCodes: projectReasons,
		Invoices: make([]domain.TravelInvoiceCheck, 0, len(checks)), ReasonCodes: append([]string{}, projectReasons...)}
	if len(checks) == 0 {
		cross.Status = combineTravelStatus(cross.Status, "UNKNOWN")
		cross.ReasonCodes = append(cross.ReasonCodes, "INVOICE_TRAVEL_FACTS_MISSING")
	}
	for _, check := range checks {
		check.Status, check.ReasonCodes = compareDeclaredInvoiceTravel(item.TravelDeclaration, check.Facts, checks)
		cross.Invoices = append(cross.Invoices, check)
		cross.Status = combineTravelStatus(cross.Status, check.Status)
		cross.ReasonCodes = append(cross.ReasonCodes, check.ReasonCodes...)
	}
	if conflictingHotelNights(checks) {
		cross.Status = "MISMATCH"
		cross.ReasonCodes = append(cross.ReasonCodes, "INVOICE_HOTEL_OVERLAP")
	}
	cross.ReasonCodes = uniqueReasons(cross.ReasonCodes)
	base.CrossCheck = cross
	base.Status = combineTravelStatus(base.Status, cross.Status)
	base.ReasonCodes = uniqueReasons(append(base.ReasonCodes, cross.ReasonCodes...))
	return base
}

// compareDeclaredProjectTravel 只使用明确城市代码或已发布映射，不猜项目自由文本。
// 参数：item为包含声明的费用，ref为权威行程，base为已有映射和摘要来源。
// 返回值：声明与权威项目逐维度合并状态及原因。
func compareDeclaredProjectTravel(item domain.Item, ref domain.ProjectReference, base domain.TravelAssessmentReference) (string, []string) {
	d := item.TravelDeclaration
	if d == nil {
		return "UNKNOWN", []string{"TRAVEL_DECLARATION_MISSING"}
	}
	facts := ref.TravelSnapshot
	if facts == nil {
		return "UNKNOWN", []string{"DECLARATION_PROJECT_FACTS_MISSING"}
	}
	destination := facts.Destination
	if !structuredCityCode(destination) && base.CityMapping != nil && base.CityMapping.Status == "MATCH" &&
		base.CityMapping.SourceCity == destination {
		destination = base.CityMapping.CityCode
	}
	status, reasons := "MATCH", []string{}
	for _, city := range []struct{ label, actual, expected string }{
		{"ORIGIN", facts.Origin, d.OriginCityCode}, {"DESTINATION", destination, d.DestinationCityCode},
	} {
		if !structuredCityCode(city.actual) {
			status = combineTravelStatus(status, "UNKNOWN")
			reasons = append(reasons, "DECLARATION_PROJECT_"+city.label+"_UNKNOWN")
		} else if city.actual != city.expected {
			status = "MISMATCH"
			reasons = append(reasons, "DECLARATION_PROJECT_"+city.label+"_MISMATCH")
		}
	}
	if !validTravelPeriod(facts.DepartureDate, facts.ReturnDate) {
		status = combineTravelStatus(status, "UNKNOWN")
		reasons = append(reasons, "DECLARATION_PROJECT_DATES_UNKNOWN")
	} else if d.DepartureDate < facts.DepartureDate || d.ReturnDate > facts.ReturnDate {
		status = "MISMATCH"
		reasons = append(reasons, "DECLARATION_PROJECT_DATES_MISMATCH")
	}
	if item.OccurredDate < d.DepartureDate || item.OccurredDate > d.ReturnDate {
		status = "MISMATCH"
		reasons = append(reasons, "TRAVEL_EXPENSE_DATE_OUTSIDE_DECLARATION")
	}
	return status, reasons
}

// compareDeclaredInvoiceTravel 接受直达和反向返程，联程中转保留待人工核实。
// 参数：d为声明，facts为当前人工票面，checks为本项全部可信票面。
// 返回值：单张票面对声明的真实状态与维度原因。
func compareDeclaredInvoiceTravel(d *domain.TravelDeclaration, facts *domain.InvoiceTravelFacts, checks []domain.TravelInvoiceCheck) (string, []string) {
	if d == nil {
		return "UNKNOWN", []string{"TRAVEL_DECLARATION_MISSING"}
	}
	if facts == nil {
		return "UNKNOWN", []string{"INVOICE_TRAVEL_FACTS_MISSING"}
	}
	status, reasons := "MATCH", []string{}
	if facts.StartDate < d.DepartureDate || facts.EndDate > d.ReturnDate {
		status, reasons = "MISMATCH", append(reasons, "INVOICE_TRAVEL_DATES_MISMATCH")
	}
	if facts.Kind == "HOTEL" {
		if facts.DestinationCityCode != d.DestinationCityCode {
			status, reasons = "MISMATCH", append(reasons, "INVOICE_TRAVEL_CITY_MISMATCH")
		}
		return status, reasons
	}
	outbound := facts.OriginCityCode == d.OriginCityCode && facts.DestinationCityCode == d.DestinationCityCode
	inbound := facts.OriginCityCode == d.DestinationCityCode && facts.DestinationCityCode == d.OriginCityCode
	if !outbound && !inbound {
		if connectedTravelSegment(d, facts, checks) {
			status = combineTravelStatus(status, "UNKNOWN")
			reasons = append(reasons, "INVOICE_MULTI_LEG_REQUIRES_CONFIRMATION")
		} else {
			status, reasons = "MISMATCH", append(reasons, "INVOICE_TRAVEL_CITY_MISMATCH")
		}
	}
	return status, reasons
}

// connectedTravelSegment 仅识别可能联程，不凭城市与日期推断同一时刻冲突。
// 参数：d为声明，facts为当前路段，checks为所有人工确认的交通票面。
// 返回值：该路段可能连接声明两端时为真，仍需人工确认时间关系。
func connectedTravelSegment(d *domain.TravelDeclaration, facts *domain.InvoiceTravelFacts, checks []domain.TravelInvoiceCheck) bool {
	for _, endpoints := range [][2]string{{d.OriginCityCode, d.DestinationCityCode}, {d.DestinationCityCode, d.OriginCityCode}} {
		if travelCityReachable(endpoints[0], facts.OriginCityCode, checks) &&
			travelCityReachable(facts.DestinationCityCode, endpoints[1], checks) {
			return true
		}
	}
	return false
}

// travelCityReachable 在有界票面城市图中判断可能连通，不生成实际行程或时序结论。
// 参数：start/end为明确城市代码，checks为当前费用交通票面集合。
// 返回值：仅沿AIR/RAIL方向可达时为真。
func travelCityReachable(start, end string, checks []domain.TravelInvoiceCheck) bool {
	queue, seen := []string{start}, map[string]bool{start: true}
	for len(queue) > 0 {
		city := queue[0]
		queue = queue[1:]
		if city == end {
			return true
		}
		for _, check := range checks {
			f := check.Facts
			if f == nil || f.Kind == "HOTEL" || f.OriginCityCode != city || seen[f.DestinationCityCode] {
				continue
			}
			seen[f.DestinationCityCode] = true
			queue = append(queue, f.DestinationCityCode)
		}
	}
	return false
}

// conflictingHotelNights 只把不同城市实际重叠的住宿晚数视为跨票矛盾。
// 参数：checks为已校验日期的人工票面集合。
// 返回值：两个HOTEL半开区间交集非空且城市不同为真；同日退住交接不冲突。
func conflictingHotelNights(checks []domain.TravelInvoiceCheck) bool {
	for i, a := range checks {
		if a.Facts == nil || a.Facts.Kind != "HOTEL" {
			continue
		}
		for _, b := range checks[i+1:] {
			if b.Facts == nil || b.Facts.Kind != "HOTEL" {
				continue
			}
			if a.Facts.DestinationCityCode != b.Facts.DestinationCityCode &&
				a.Facts.StartDate < b.Facts.EndDate && b.Facts.StartDate < a.Facts.EndDate {
				return true
			}
		}
	}
	return false
}

// combineTravelStatus 保持矛盾优先、未知次之，不允许部分MATCH掩盖缺资料。
// 参数：a/b为两个服务端派生状态。
// 返回值：保留更严格信息状态的合并结果。
func combineTravelStatus(a, b string) string {
	if a == "MISMATCH" || b == "MISMATCH" {
		return "MISMATCH"
	}
	if a != "MATCH" || b != "MATCH" {
		return "UNKNOWN"
	}
	return "MATCH"
}

// frozenTravelCrossReference 付款重核采用本轮冻结交叉结论而非用当前票面重造旧结论。
// 参数：item为已批准费用，old为冻结项目引用，fallback为旧版项目维度计算。
// 返回值：存在新式冻结交叉时返回原结论，否则保留历史兼容回退。
func frozenTravelCrossReference(item domain.Item, old domain.ProjectReference, fallback domain.TravelAssessmentReference) domain.TravelAssessmentReference {
	if item.TravelAssessment != nil {
		for _, ref := range item.TravelAssessment.References {
			if ref.ProjectID == old.ProjectID && ref.ServiceItemID == old.ServiceItemID && ref.CrossCheck != nil {
				return ref
			}
		}
	}
	return fallback
}

// changedTravelInvoiceFacts 比较不可变票面事实摘要，防止同状态原因掩盖换票或更正。
// 参数：current/previous分别为当前和已批准交叉结果。
// 返回值：交叉范围被新增、丢失或任何用票事实摘要变化时为真。
func changedTravelInvoiceFacts(current, previous *domain.TravelCrossCheck) bool {
	if current == nil || previous == nil {
		return current != nil || previous != nil
	}
	if len(current.Invoices) != len(previous.Invoices) {
		return true
	}
	old := map[string]string{}
	for _, inv := range previous.Invoices {
		old[inv.InvoiceID] = inv.FactDigest
	}
	for _, inv := range current.Invoices {
		value, exists := old[inv.InvoiceID]
		if !exists || value != inv.FactDigest {
			return true
		}
	}
	return false
}
