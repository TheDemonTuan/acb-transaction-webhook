package acb

import (
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

const historyOwnershipCandidateLimit = 8

type historyFormOwnershipDiagnostic struct {
	Forms      int                             `json:"forms"`
	Controls   int                             `json:"controls"`
	Candidates []historyFormOwnershipCandidate `json:"candidates"`
	Truncated  bool                            `json:"truncated"`
}

type historyFormOwnershipCandidate struct {
	Ordinal               int      `json:"ordinal"`
	ActionKind            string   `json:"action_kind"`
	OperationKind         string   `json:"operation_kind"`
	DOMKeys               []string `json:"dom_keys"`
	SourceKeys            []string `json:"source_keys"`
	DOMHasSession         bool     `json:"dom_has_session"`
	DOMHasState           bool     `json:"dom_has_state"`
	SourceHasSession      bool     `json:"source_has_session"`
	SourceHasState        bool     `json:"source_has_state"`
	OpenedInTable         bool     `json:"opened_in_table"`
	Balanced              bool     `json:"balanced"`
	NestedForm            bool     `json:"nested_form"`
	UnsupportedContext    bool     `json:"unsupported_context"`
	ExternalOwnerControls int      `json:"external_owner_controls"`
}

type historySourceFormSpan struct {
	start, end int
	id, action string
	candidate  historyFormOwnershipCandidate
}

func historyOwnershipKey(name string) bool {
	switch name {
	case "dse_applicationId", "dse_operationName", "dse_pageId", "dse_processorState", "dse_errorPage", "dse_nextEventName", "dse_sessionId", "dse_processorId", "dse_processorIdForGenMenu", "AccountNbr", "virtualAccount", "storeName", "CheckRef", "EdtRef", "CheckDoiUng", "activeDatetimeYN", "FromDate", "ToDate", "activeDatetimeByMonth", "MonthCurr", "YearCurr":
		return true
	}
	return false
}

func historyOwnershipAction(action string) string {
	if action == "" {
		return "EMPTY"
	}
	u, err := url.Parse(action)
	if err == nil && u.Path == "/acbib/Request" && u.RawQuery == "" && u.Fragment == "" && u.User == nil && (u.Host == "" && u.Scheme == "" || strings.EqualFold(u.Hostname(), OfficialHost) && u.Scheme == "https") {
		return "REQUEST"
	}
	return "OTHER"
}

func historyOwnershipOperation(value string) string {
	switch value {
	case "":
		return "MISSING"
	case "ibkacctDetailProc":
		return "DETAIL"
	case "ibkacctSumProc":
		return "SUMMARY"
	default:
		return "OTHER"
	}
}

func historyOwnershipFields(root *html.Node, owner string) map[string]string {
	fields := make(map[string]string)
	if root == nil {
		return fields
	}
	walk(root, func(n *html.Node) {
		if hasAttr(n, "form") && (owner == "" || attrVal(n, "form") != owner) {
			return
		}
		if name, value, ok := successfulHistoryControl(n); ok && historyOwnershipKey(name) {
			fields[name] = value
		}
	})
	return fields
}

func historyOwnershipKeys(fields map[string]string) []string {
	keys := make([]string, 0, len(fields))
	for name := range fields {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// This is evidence only. Source spans never supply outbound request fields.
// Tokenizer raw byte lengths delimit source slices without copying markup.
func diagnoseHistoryFormOwnership(markup string, doc *html.Node) historyFormOwnershipDiagnostic {
	result := historyFormOwnershipDiagnostic{}
	if doc == nil {
		return result
	}
	var domForms []*html.Node
	ids := make(map[string][]*html.Node)
	var explicit []*html.Node
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if id := attrVal(n, "id"); id != "" {
			ids[id] = append(ids[id], n)
		}
		if n.Data == "form" {
			domForms = append(domForms, n)
		}
		switch n.Data {
		case "input", "select", "textarea":
			result.Controls++
			if hasAttr(n, "form") {
				explicit = append(explicit, n)
			}
		}
	})
	z := html.NewTokenizer(strings.NewReader(markup))
	var spans []historySourceFormSpan
	var active []int
	var stack []string
	offset := 0
	unsupported := func(tag string) bool { return tag == "select" || tag == "template" || tag == "svg" || tag == "math" }
	for {
		kind := z.Next()
		start := offset
		offset += len(z.Raw())
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken && kind != html.EndTagToken {
			continue
		}
		token := z.Token()
		tag := token.Data
		if kind == html.EndTagToken {
			if tag == "form" && len(active) > 0 {
				index := active[len(active)-1]
				if index >= 0 {
					spans[index].end = offset
					spans[index].candidate.Balanced = true
				}
				active = active[:len(active)-1]
			}
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == tag {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		if unsupported(tag) {
			for _, index := range active {
				if index >= 0 {
					spans[index].candidate.UnsupportedContext = true
				}
			}
		}
		if tag == "form" {
			result.Forms++
			if result.Forms > historyOwnershipCandidateLimit {
				result.Truncated = true
			}
			span := historySourceFormSpan{start: start, candidate: historyFormOwnershipCandidate{Ordinal: result.Forms}}
			for _, a := range token.Attr {
				switch a.Key {
				case "id":
					span.id = a.Val
				case "action":
					span.action = a.Val
				}
			}
			span.candidate.ActionKind = historyOwnershipAction(span.action)
			for _, context := range stack {
				if context == "table" {
					span.candidate.OpenedInTable = true
				}
				if unsupported(context) {
					span.candidate.UnsupportedContext = true
				}
			}
			if len(active) > 0 {
				span.candidate.NestedForm = true
				for _, index := range active {
					if index >= 0 {
						spans[index].candidate.NestedForm = true
					}
				}
			}
			if len(spans) < historyOwnershipCandidateLimit {
				spans = append(spans, span)
				active = append(active, len(spans)-1)
			} else {
				active = append(active, -1)
			}
		}
		if kind == html.StartTagToken {
			switch tag {
			case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
			default:
				stack = append(stack, tag)
			}
		}
	}
	limit := len(spans)
	if limit > historyOwnershipCandidateLimit {
		limit = historyOwnershipCandidateLimit
	}
	result.Candidates = make([]historyFormOwnershipCandidate, 0, limit)
	for i := range limit {
		span := spans[i]
		candidate := span.candidate
		if !candidate.Balanced || candidate.NestedForm {
			candidate.UnsupportedContext = true
		}
		var dom *html.Node
		if span.id != "" {
			if owners := ids[span.id]; len(owners) == 1 && owners[0].Data == "form" {
				dom = owners[0]
			} else {
				candidate.UnsupportedContext = true
			}
		} else if len(domForms) == result.Forms && i < len(domForms) {
			dom = domForms[i]
		} else {
			candidate.UnsupportedContext = true
		}
		domFields := historyOwnershipFields(dom, span.id)
		candidate.DOMKeys = historyOwnershipKeys(domFields)
		candidate.DOMHasSession = domFields["dse_sessionId"] != ""
		candidate.DOMHasState = domFields["dse_processorState"] != ""
		candidate.OperationKind = historyOwnershipOperation(domFields["dse_operationName"])
		for _, control := range explicit {
			owner := attrVal(control, "form")
			if owner == "" || owner != span.id {
				continue
			}
			if len(ids[owner]) != 1 || ids[owner][0].Data != "form" {
				candidate.UnsupportedContext = true
				continue
			}
			inside := false
			for parent := control.Parent; parent != nil; parent = parent.Parent {
				if parent == dom {
					inside = true
					break
				}
			}
			if !inside {
				candidate.ExternalOwnerControls++
			}
		}
		if candidate.Balanced && !candidate.NestedForm {
			// Parse only this form's source span in a neutral context. The original
			// table ancestry is separately reported; no document-wide field union.
			if source, err := html.Parse(strings.NewReader(markup[span.start:span.end])); err == nil {
				walk(source, func(n *html.Node) {
					if !hasAttr(n, "form") {
						return
					}
					owner := attrVal(n, "form")
					if owner == "" || len(ids[owner]) != 1 || ids[owner][0].Data != "form" {
						candidate.UnsupportedContext = true
					}
				})
				fields := historyOwnershipFields(source, span.id)
				candidate.SourceKeys = historyOwnershipKeys(fields)
				candidate.SourceHasSession = fields["dse_sessionId"] != ""
				candidate.SourceHasState = fields["dse_processorState"] != ""
				if candidate.OperationKind == "MISSING" {
					candidate.OperationKind = historyOwnershipOperation(fields["dse_operationName"])
				}
			}
		}
		result.Candidates = append(result.Candidates, candidate)
	}
	return result
}
