package acb

import (
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// SafePath removes query parameters and fragments before a URL is logged.
func SafePath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

// PageStructureDiagnostic reports structural features of an ACB page without
// leaking values, cookies, account numbers, or PII.
type PageStructureDiagnostic struct {
	TableCount              int      `json:"table_count"`
	HeaderColumnsRecognized []string `json:"header_columns_recognized"`
	BodyRowCount            int      `json:"body_row_count"`
	EmptyMarkerPresent      bool     `json:"empty_marker_present"`
	TotalRowsExtracted      int      `json:"total_rows_extracted"`
	TotalRowsFound          bool     `json:"total_rows_found"`
	FormAction              string   `json:"form_action"`
	FormKeys                []string `json:"form_keys"`
}

// DiagnosePageStructure inspects markup structure safely.
func DiagnosePageStructure(markup string) PageStructureDiagnostic {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return PageStructureDiagnostic{
			HeaderColumnsRecognized: []string{},
			FormKeys:                []string{},
		}
	}

	diag := PageStructureDiagnostic{
		HeaderColumnsRecognized: []string{},
		FormKeys:                []string{},
	}

	var tables []*html.Node
	walk(doc, func(node *html.Node) {
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "table") {
			tables = append(tables, node)
		}
	})
	diag.TableCount = len(tables)

	for _, table := range tables {
		rows := tableRows(table)
		headerIndex, _ := historyHeader(rows)
		if headerIndex >= 0 {
			if len(diag.HeaderColumnsRecognized) == 0 {
				diag.HeaderColumnsRecognized = extractRecognizedColumns(rows[headerIndex])
			}
			for i := headerIndex + 1; i < len(rows); i++ {
				row := rows[i]
				if len(row) == 0 || allBlank(row) {
					continue
				}
				rowText := strings.ToLower(strings.Join(row, " "))
				if containsAny(rowText, "khong co giao dich", "không có giao dịch", "khong co du lieu", "không có dữ liệu", "no transaction", "chua co giao dich", "chưa có giao dịch") {
					diag.EmptyMarkerPresent = true
					continue
				}
				if isTableFooter(rowText) {
					continue
				}
				diag.BodyRowCount++
			}
		} else {
			for _, r := range rows {
				rowText := strings.ToLower(strings.Join(r, " "))
				if containsAny(rowText, "khong co giao dich", "không có giao dịch", "khong co du lieu", "không có dữ liệu", "no transaction", "chua co giao dich", "chưa có giao dịch") {
					diag.EmptyMarkerPresent = true
				}
			}
		}
	}

	domText := domVisibleText(doc)
	normDomText := strings.ToLower(domText)
	if !diag.EmptyMarkerPresent && containsAny(normDomText, "khong co giao dich", "không có giao dịch", "khong co du lieu", "không có dữ liệu", "no transaction", "chua co giao dich", "chưa có giao dịch") {
		diag.EmptyMarkerPresent = true
	}

	totalRows, found := extractTotalRows(domText)
	diag.TotalRowsExtracted = totalRows
	diag.TotalRowsFound = found

	if form, err := ExtractHistoryForm(markup); err == nil {
		diag.FormAction = SafePath(form.Action)
		keys := make([]string, 0, len(form.Fields))
		for k := range form.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		diag.FormKeys = keys
	}

	return diag
}

// InspectPageStructure is an alias for DiagnosePageStructure.
func InspectPageStructure(markup string) PageStructureDiagnostic {
	return DiagnosePageStructure(markup)
}

func extractRecognizedColumns(header []string) []string {
	var recognized []string
	for _, name := range header {
		switch normalized(name) {
		case "sogd", "sogiaodich", "sogiao dich", "transactionnumber":
			recognized = append(recognized, "sogd")
		case "ngayhieuluc", "effective date":
			recognized = append(recognized, "ngayhieuluc")
		case "ngaygiaodich", "transaction date":
			recognized = append(recognized, "ngaygiaodich")
		case "ghino", "debit":
			recognized = append(recognized, "ghino")
		case "ghico", "credit":
			recognized = append(recognized, "ghico")
		case "sodu", "balance":
			recognized = append(recognized, "sodu")
		case "noidunggiaodich", "description", "content":
			recognized = append(recognized, "noidunggiaodich")
		}
	}
	return recognized
}
