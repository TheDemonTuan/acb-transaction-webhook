package acb

import (
	"log/slog"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Temporary incident diagnostic: log only dates, fixed selector values and counts.
// Never emit form state, accounts, amounts, transaction identifiers or raw markup.
func (c *Client) logHistoryContract(fields map[string]string, response Response) {
	if response.Kind != HistoryPage && response.Kind != AccountDetailPage {
		return
	}
	key := safeHistoryControl("FromDate", fields["FromDate"]) + ":" + safeHistoryControl("ToDate", fields["ToDate"])
	if c.historyDiagnostics[key] >= 2 || len(c.historyDiagnostics) >= 8 {
		return
	}
	if c.historyDiagnostics == nil {
		c.historyDiagnostics = make(map[string]int)
	}
	c.historyDiagnostics[key]++
	page, parseErr := ParseHistoryPage(response.Body)
	form, formErr := ExtractHistoryForm(response.Body)
	controls := make(map[string][]map[string]any)
	if doc, err := html.Parse(strings.NewReader(response.Body)); err == nil {
		walk(doc, func(n *html.Node) {
			if n.Type != html.ElementNode || n.Data != "input" {
				return
			}
			name := attrVal(n, "name")
			switch name {
			case "FromDate", "ToDate", "activeDatetimeYN", "activeDatetimeByMonth", "CheckRef", "CheckDoiUng", "dse_nextEventName":
				controls[name] = append(controls[name], map[string]any{
					"value":   safeHistoryControl(name, attrVal(n, "value")),
					"checked": hasAttr(n, "checked"), "disabled": hasAttr(n, "disabled"),
				})
			}
		})
	}
	request := make(map[string]string)
	responseFields := make(map[string]string)
	for _, name := range []string{"FromDate", "ToDate", "activeDatetimeYN", "activeDatetimeByMonth", "CheckRef", "CheckDoiUng", "dse_nextEventName"} {
		request[name] = safeHistoryControl(name, fields[name])
		responseFields[name] = safeHistoryControl(name, form.Fields[name])
	}
	slog.Info("ACB history contract diagnostic", "request", request, "status", response.StatusCode,
		"response_form", responseFields, "form_valid", formErr == nil, "controls", controls,
		"parse_valid", parseErr == nil, "rows", len(page.Transactions),
		"transaction_days", HistoryDayCounts(page.Transactions), "effective_days", HistoryEffectiveDayCounts(page.Transactions),
		"has_next", page.HasNext, "total_rows", page.TotalRows, "structure", DiagnosePageStructure(response.Body))
}

func safeHistoryControl(name, value string) string {
	if value == "" {
		return ""
	}
	if name == "FromDate" || name == "ToDate" {
		for _, layout := range []string{"02/01/2006", "2006-01-02", "02-01-2006"} {
			if d, err := time.Parse(layout, value); err == nil && d.Format(layout) == value {
				return value
			}
		}
		return "REDACTED"
	}
	switch value {
	case "Y", "N", "true", "false", "byDate", "byMonth", "nextPage", "previousPage":
		return value
	default:
		return "REDACTED"
	}
}
