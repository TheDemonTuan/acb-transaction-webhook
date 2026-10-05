package acb

import (
	"strings"
	"testing"
)

func tableOwnedForm(id, account string) string {
	return `<table><form id="` + id + `" action="/acbib/Request"><input type="hidden" name="dse_operationName" value="ibkacctDetailProc"><input type="hidden" name="dse_sessionId" value="synthetic-session"><input type="hidden" name="dse_processorState" value="synthetic-state"><input type="hidden" name="AccountNbr" value="` + account + `"><tr><td><input name="FromDate" value="05/10/2026"><input name="ToDate" value="05/10/2026"><input type="radio" name="activeDatetimeYN" value="N" checked><input type="radio" name="activeDatetimeYN" value="Y"><input type="checkbox" name="CheckRef" value="true"><select name="AccountNbr" disabled><option selected>87654321</option></select></td></tr></form></table>`
}

func TestExtractHistoryFormQualifiedTableOwnership(t *testing.T) {
	date := tableOwnedForm("date", "12345678")
	month := `<table><form id="month" action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_sessionId" value="month-session"><input name="dse_processorState" value="month-state"><input name="AccountNbr" value="87654321"><tr><td><input name="MonthCurr" value="10"></td></tr></form></table>`
	state, err := ExtractHistoryForm(month + date)
	if err != nil {
		t.Fatal(err)
	}
	if state.Action != "/acbib/Request" || state.Fields["AccountNbr"] != "12345678" || state.Fields["dse_processorState"] != "synthetic-state" || state.Fields["dse_sessionId"] != "synthetic-session" || state.Fields["FromDate"] != recoveryDate || state.Fields["activeDatetimeYN"] != "N" {
		t.Fatal("date form lost exact owner, selection or successful controls")
	}
	if _, exists := state.Fields["MonthCurr"]; exists {
		t.Fatal("merged month controls into date form")
	}
	if _, exists := state.Fields["CheckRef"]; exists {
		t.Fatal("unchecked checkbox became successful")
	}
	changed := strings.Replace(date, `value="N" checked`, `value="N"`, 1)
	changed = strings.Replace(changed, `value="Y">`, `value="Y" checked>`, 1)
	state, err = ExtractHistoryForm(changed)
	if err != nil || state.Fields["activeDatetimeYN"] != "Y" {
		t.Fatal("changed successful radio selection was ignored")
	}
}

func TestExtractHistoryFormTableOwnershipFailsClosed(t *testing.T) {
	base := tableOwnedForm("date", "12345678")
	cases := map[string]string{
		"unbalanced":         strings.Replace(base, "</form>", "", 1),
		"nested":             strings.Replace(base, "<tr>", "<form></form><tr>", 1),
		"template":           strings.Replace(base, "<tr>", "<template></template><tr>", 1),
		"foreign":            strings.Replace(base, "<tr>", "<svg></svg><tr>", 1),
		"select_start":       "<select>" + base + "</select>",
		"missing_session":    strings.Replace(base, `value="synthetic-session"`, `value=""`, 1),
		"missing_state":      strings.Replace(base, `value="synthetic-state"`, `value=""`, 1),
		"summary":            strings.Replace(base, "ibkacctDetailProc", "ibkacctSumProc", 1),
		"duplicate_owner":    `<div id="date"></div>` + base,
		"external_decoy":     base + `<input form="date" name="AccountNbr" value="87654321">`,
		"explicit_foreign":   strings.Replace(base, `name="AccountNbr" value="12345678"`, `name="AccountNbr" form="other" value="12345678"`, 1) + `<form id="other"></form>`,
		"unresolved_owner":   strings.Replace(base, `name="AccountNbr" value="12345678"`, `name="AccountNbr" form="missing" value="12345678"`, 1),
		"empty_owner":        strings.Replace(base, `name="AccountNbr" value="12345678"`, `name="AccountNbr" form="" value="12345678"`, 1),
		"ambiguous_accounts": base + tableOwnedForm("other", "87654321"),
		"truncated":          base + strings.Repeat(`<form></form>`, 8),
	}
	for name, markup := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractHistoryForm(markup); err == nil {
				t.Fatal("unsafe source ownership became outbound state")
			}
		})
	}
}
