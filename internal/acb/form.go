package acb

import (
	"errors"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type FormState struct {
	Action string
	Fields map[string]string
}

const historyDateLayout = "02/01/2006"

func validateHistoryDateRange(fromDate, toDate string) error {
	from, err := time.Parse(historyDateLayout, fromDate)
	if err != nil || from.Format(historyDateLayout) != fromDate {
		return errors.New("ACB history request has an invalid FromDate")
	}
	to, err := time.Parse(historyDateLayout, toDate)
	if err != nil || to.Format(historyDateLayout) != toDate {
		return errors.New("ACB history request has an invalid ToDate")
	}
	if from.After(to) {
		return errors.New("ACB history request has an invalid date range")
	}
	return nil
}

// PinDateRangePreservingPagination clones fields and changes only the date range.
func PinDateRangePreservingPagination(fields map[string]string, fromDate, toDate string) (map[string]string, error) {
	if err := validateHistoryDateRange(fromDate, toDate); err != nil {
		return nil, err
	}
	pinned := cloneFields(fields)
	pinned["FromDate"] = fromDate
	pinned["ToDate"] = toDate
	pinned["activeDatetimeYN"] = "N"
	delete(pinned, "_explicitRange")
	delete(pinned, "activeDatetimeByMonth")
	delete(pinned, "MonthCurr")
	delete(pinned, "YearCurr")
	return pinned, nil
}

// PrepareHistoryFieldsWithRange converts form fields into an explicit by-date query with specified date range.
func PrepareHistoryFieldsWithRange(fields map[string]string, fromDate, toDate string) (map[string]string, error) {
	if err := validateHistoryDateRange(fromDate, toDate); err != nil {
		return nil, err
	}
	prepared := cloneFields(fields)
	delete(prepared, "_raw")
	delete(prepared, "_explicitRange")
	if prepared["dse_operationName"] == "" || prepared["dse_processorState"] == "" {
		return nil, errors.New("ACB history request is missing current form state")
	}
	prepared["dse_nextEventName"] = "byDate"
	prepared["activeDatetimeYN"] = "N"
	prepared["FromDate"] = fromDate
	prepared["ToDate"] = toDate
	prepared["CheckRef"] = "false"
	prepared["CheckDoiUng"] = "false"

	// These controls belong to the mutually exclusive by-month query.
	for _, name := range []string{"activeDatetimeByMonth", "MonthCurr", "YearCurr"} {
		delete(prepared, name)
	}
	return prepared, nil
}

// PrepareHistoryFieldsForDate converts a form into a by-date query for one
// caller-selected local date. It is used when a multi-page poll must keep its
// original Asia/Ho_Chi_Minh day across requests.
func PrepareHistoryFieldsForDate(fields map[string]string, date string) (map[string]string, error) {
	if err := validateHistoryDateRange(date, date); err != nil {
		return nil, err
	}
	if fields != nil && fields["_raw"] == "true" {
		prepared := cloneFields(fields)
		delete(prepared, "_raw")
		if prepared["_explicitRange"] == "true" {
			return nil, errors.New("ACB realtime request cannot use an explicit range")
		}
		return PinDateRangePreservingPagination(prepared, date, date)
	}
	return PrepareHistoryFieldsWithRange(fields, date, date)
}

// PrepareHistoryFields converts the current ACB account-detail form into an
// explicit by-date query for today in the configured zone, or uses explicit
// FromDate/ToDate if _explicitRange is set.
func PrepareHistoryFields(fields map[string]string, now time.Time, location *time.Location) (map[string]string, error) {
	if fields != nil && fields["_raw"] == "true" {
		res := cloneFields(fields)
		delete(res, "_raw")
		if res["_explicitRange"] == "true" {
			if res["FromDate"] == "" || res["ToDate"] == "" {
				return nil, errors.New("ACB history request explicit range is incomplete")
			}
			if err := validateHistoryDateRange(res["FromDate"], res["ToDate"]); err != nil {
				return nil, err
			}
			delete(res, "_explicitRange")
			return res, nil
		}
		if location == nil {
			return nil, errors.New("ACB history timezone is required")
		}
		today := now.In(location).Format(historyDateLayout)
		return PinDateRangePreservingPagination(res, today, today)
	}
	if location == nil {
		return nil, errors.New("ACB history timezone is required")
	}
	today := now.In(location)
	fromDate := today.Format(historyDateLayout)
	toDate := fromDate
	if fields != nil && fields["_explicitRange"] == "true" {
		if fields["FromDate"] == "" || fields["ToDate"] == "" {
			return nil, errors.New("ACB history request explicit range is incomplete")
		}
		fromDate = fields["FromDate"]
		toDate = fields["ToDate"]
	}
	res, err := PrepareHistoryFieldsWithRange(fields, fromDate, toDate)
	if err == nil && res != nil {
		delete(res, "_explicitRange")
	}
	return res, err
}

// ExtractHistoryForm reads the current server-generated form state. It never
// accepts a state saved from a prior session.
func ExtractHistoryForm(markup string) (FormState, error) {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return FormState{}, err
	}

	var forms []*html.Node
	var findForms func(*html.Node)
	findForms = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "form") {
			forms = append(forms, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findForms(c)
		}
	}
	findForms(doc)

	extractFields := func(root *html.Node) FormState {
		state := FormState{Fields: make(map[string]string)}
		if root.Type == html.ElementNode && strings.EqualFold(root.Data, "form") {
			for _, attr := range root.Attr {
				if strings.EqualFold(attr.Key, "action") {
					state.Action = attr.Val
				}
			}
		}
		var visit func(*html.Node)
		visit = func(node *html.Node) {
			if node.Type == html.ElementNode {
				switch strings.ToLower(node.Data) {
				case "input", "textarea", "select":
					name, value, ok := successfulHistoryControl(node)
					if !ok {
						return
					}
					state.Fields[name] = value
				}
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				visit(child)
			}
		}
		visit(root)
		return state
	}

	var bestState FormState
	var bestScore int = -1
	for _, f := range forms {
		s := extractFields(f)
		if score := historyFormScore(s); score > bestScore {
			bestScore = score
			bestState = s
		}
	}
	if bestScore >= 0 {
		return bestState, nil
	}

	if len(forms) > 0 {
		if state, ok := legacyTableHistoryForm(markup, doc); ok {
			return state, nil
		}
		return FormState{}, errors.New("ACB account form state is incomplete")
	}
	whole := extractFields(doc)
	if whole.Action == "" {
		whole.Action = "/acbib/Request"
	}
	if whole.Fields["dse_processorState"] != "" && whole.Fields["dse_operationName"] != "" {
		return whole, nil
	}

	return FormState{}, errors.New("ACB account form state is incomplete")
}

// successfulHistoryControl preserves the extraction successful-control rules.
func successfulHistoryControl(node *html.Node) (string, string, bool) {
	if node == nil || node.Type != html.ElementNode {
		return "", "", false
	}
	tag := strings.ToLower(node.Data)
	if tag != "input" && tag != "textarea" && tag != "select" {
		return "", "", false
	}
	var name, value, inputType string
	var disabled, checked, valuePresent bool
	for _, attr := range node.Attr {
		switch strings.ToLower(attr.Key) {
		case "name":
			name = attr.Val
		case "value":
			value, valuePresent = attr.Val, true
		case "type":
			inputType = strings.ToLower(attr.Val)
		case "disabled":
			disabled = true
		case "checked":
			checked = true
		}
	}
	if name == "" || disabled {
		return "", "", false
	}
	switch tag {
	case "input":
		switch inputType {
		case "submit", "button", "reset", "image", "file":
			return "", "", false
		case "radio", "checkbox":
			if !checked {
				return "", "", false
			}
			if !valuePresent {
				value = "on"
			}
		}
	case "textarea":
		var content strings.Builder
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				content.WriteString(child.Data)
			}
		}
		value = content.String()
	case "select":
		var found bool
		var scanOption func(*html.Node)
		scanOption = func(opt *html.Node) {
			if opt.Type == html.ElementNode {
				for _, attr := range opt.Attr {
					if strings.EqualFold(attr.Key, "disabled") {
						return
					}
				}
				if strings.EqualFold(opt.Data, "option") {
					optionValue := nodeText(opt)
					isSelected := false
					for _, attr := range opt.Attr {
						if strings.EqualFold(attr.Key, "value") {
							optionValue = attr.Val
						}
						if strings.EqualFold(attr.Key, "selected") {
							isSelected = true
						}
					}
					if !found || isSelected {
						value = optionValue
					}
					found = true
					return
				}
			}
			for c := opt.FirstChild; c != nil; c = c.NextSibling {
				scanOption(c)
			}
		}
		scanOption(node)
		if !found {
			return "", "", false
		}
	}
	return name, value, true
}

func historyFormScore(state FormState) int {
	if state.Action == "" || state.Fields["dse_processorState"] == "" || state.Fields["dse_operationName"] == "" {
		return -1
	}
	score := 1
	if state.Fields["dse_operationName"] == "ibkacctDetailProc" {
		score += 5
	}
	if state.Fields["dse_nextEventName"] == "byDate" || state.Fields["FromDate"] != "" && state.Fields["ToDate"] != "" {
		score += 10
	}
	if state.Fields["AccountNbr"] != "" {
		score += 2
	}
	return score
}

// x/net/html pops a table-opened form from its stack. Native Chromium still
// owns its following controls. Recover only a complete, bounded source span;
// never combine tokens/accounts across forms or bless ambiguous ownership.
func legacyTableHistoryForm(markup string, doc *html.Node) (FormState, bool) {
	spans, _, truncated := historySourceFormSpans(markup)
	if truncated {
		return FormState{}, false
	}
	ids := make(map[string][]*html.Node)
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && attrVal(n, "id") != "" {
			ids[attrVal(n, "id")] = append(ids[attrVal(n, "id")], n)
		}
	})
	bestScore := -1
	var best FormState
	ambiguous := false
	for _, span := range spans {
		shape := span.candidate
		if !shape.OpenedInTable || !shape.Balanced || shape.NestedForm || shape.UnsupportedContext || span.action == "" {
			continue
		}
		if span.id != "" && (len(ids[span.id]) != 1 || ids[span.id][0].Data != "form") {
			continue
		}
		source, err := html.Parse(strings.NewReader(markup[span.start:span.end]))
		if err != nil {
			continue
		}
		conflict := false
		walk(source, func(n *html.Node) {
			if !hasAttr(n, "form") {
				return
			}
			name, _, successful := successfulHistoryControl(n)
			if !successful || !historyOwnershipKey(name) {
				return
			}
			owner := attrVal(n, "form")
			if owner == "" || owner != span.id || len(ids[owner]) != 1 || ids[owner][0].Data != "form" {
				conflict = true
			}
		})
		// An explicit external successful control could supply/override a
		// critical field. This source-only fallback cannot prove its ordering.
		if span.id != "" {
			walk(doc, func(n *html.Node) {
				name, _, successful := successfulHistoryControl(n)
				if successful && historyOwnershipKey(name) && hasAttr(n, "form") && attrVal(n, "form") == span.id {
					conflict = true
				}
			})
		}
		if conflict {
			continue
		}
		fields := historyOwnershipFields(source, span.id)
		if fields["dse_operationName"] != "ibkacctDetailProc" || fields["dse_sessionId"] == "" || fields["dse_processorState"] == "" {
			continue
		}
		state := FormState{Action: span.action, Fields: fields}
		score := historyFormScore(state)
		if score > bestScore {
			best, bestScore, ambiguous = state, score, false
		} else if score == bestScore && fields["AccountNbr"] != best.Fields["AccountNbr"] {
			ambiguous = true
		}
	}
	return best, bestScore >= 0 && !ambiguous
}
