package acb

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestHistoryContractDiagnosticRedactsAndBounds(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	client := &Client{}
	fields := map[string]string{"FromDate": "26/09/2026", "ToDate": "26/09/2026", "activeDatetimeYN": "N", "AccountNbr": "PRIVATE_ACCOUNT", "dse_sessionId": "PRIVATE_SESSION"}
	response := Response{StatusCode: 200, Kind: HistoryPage, Body: `<form id="PRIVATE_FORM_ID" action="/PRIVATE_PATH?token=PRIVATE_TOKEN"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="PRIVATE_STATE"><input name="PRIVATE_CONTROL" value="PRIVATE_COOKIE"><input name="FromDate" value="PRIVATE_DATE"><input name="activeDatetimeYN" value="PRIVATE_SELECTOR"></form><table><tr><th>Ngày giao dịch</th><th>Ngày hiệu lực</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Nội dung giao dịch</th></tr><tr><td>26/09/2026</td><td>26/09/2026</td><td>PRIVATE_ID</td><td>123456789</td><td>0</td><td>PRIVATE_DESCRIPTION</td></tr></table>`}
	for range 20 {
		client.logHistoryContract("history", fields, response)
	}
	if strings.Contains(output.String(), "PRIVATE_") || strings.Contains(output.String(), "123456789") {
		t.Fatal("diagnostic leaked sensitive fixture content")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two bounded observations, got %d", len(lines))
	}
	var got struct {
		Request  map[string]string `json:"request"`
		Response map[string]string `json:"response_form"`
		Rows     int               `json:"rows"`
		Days     []HistoryDayCount `json:"transaction_days"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Request["FromDate"] != "26/09/2026" || got.Response["FromDate"] != "REDACTED" || got.Response["activeDatetimeYN"] != "REDACTED" || got.Rows != 1 || len(got.Days) != 1 || got.Days[0].Day != "2026-09-26" || got.Days[0].Rows != 1 {
		t.Fatalf("incorrect safe aggregate: %+v", got)
	}
}

func TestHistoryFormOwnershipEvidence(t *testing.T) {
	controls := `<input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_sessionId" value="PRIVATE_TOKEN"><input name="dse_processorState" value="PRIVATE_STATE"><input name="AccountNbr" value="PRIVATE_ACCOUNT"><input name="not_allowed" value="PRIVATE_DESCRIPTION">`
	cases := []struct {
		name, markup                                                string
		domState, sourceState, table, balanced, nested, unsupported bool
		external                                                    int
		action                                                      string
	}{
		{"normal", `<form id="PRIVATE_FORM_ID" action="/acbib/Request">` + controls + `</form>`, true, true, false, true, false, false, 0, "REQUEST"},
		{"table", `<table><form action="/acbib/Request">` + controls + `<tr><td><input name="FromDate" value="05/10/2026"></td></tr></form></table>`, false, true, true, true, false, false, 0, "REQUEST"},
		{"missing", `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"></form>`, false, false, false, true, false, false, 0, "REQUEST"},
		{"external", `<form id="PRIVATE_FORM_ID" action="/acbib/Request">` + controls + `</form><input form="PRIVATE_FORM_ID" name="FromDate" value="PRIVATE_DATE">`, true, true, false, true, false, false, 1, "REQUEST"},
		{"foreign_owner", `<form id="PRIVATE_FORM_ID" action="/private/path?secret=PRIVATE_TOKEN"><input name="dse_processorState" form="PRIVATE_OTHER_ID" value="PRIVATE_STATE"></form><form id="PRIVATE_OTHER_ID"></form>`, false, false, false, true, false, false, 0, "OTHER"},
		{"nested", `<form>` + controls + `<form></form></form>`, false, false, false, true, true, true, 0, "EMPTY"},
		{"template", `<form><template>` + controls + `</template></form>`, true, true, false, true, false, true, 0, "EMPTY"},
		{"foreign", `<svg><form>` + controls + `</form></svg>`, true, true, false, true, false, true, 0, "EMPTY"},
		{"select", `<select><form>` + controls + `</form></select>`, false, true, false, true, false, true, 0, "EMPTY"},
		{"unbalanced", `<form>` + controls, true, false, false, false, false, true, 0, "EMPTY"},
		{"duplicate_id", `<form id="PRIVATE_FORM_ID">` + controls + `</form><form id="PRIVATE_FORM_ID"></form>`, false, true, false, true, false, true, 0, "EMPTY"},
		{"unresolved_owner", `<form id="PRIVATE_FORM_ID"><input name="dse_processorState" form="PRIVATE_UNKNOWN_ID" value="PRIVATE_STATE"></form>`, false, false, false, true, false, true, 0, "EMPTY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(tc.markup))
			if err != nil {
				t.Fatal(err)
			}
			got := diagnoseHistoryFormOwnership(tc.markup, doc)
			if len(got.Candidates) == 0 {
				t.Fatal("missing source candidate")
			}
			c := got.Candidates[0]
			if c.DOMHasState != tc.domState || c.SourceHasState != tc.sourceState || c.OpenedInTable != tc.table || c.Balanced != tc.balanced || c.NestedForm != tc.nested || c.UnsupportedContext != tc.unsupported || c.ExternalOwnerControls != tc.external || c.ActionKind != tc.action {
				t.Fatalf("ownership flags: %+v", c)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"PRIVATE_", "private/path", "not_allowed"} {
				if strings.Contains(string(encoded), private) {
					t.Fatal("ownership diagnostic leaked private or unallowlisted data")
				}
			}
			if tc.name == "table" {
				if c.OperationKind != "DETAIL" || !c.SourceHasSession || c.DOMHasSession {
					t.Fatalf("source completeness lost: %+v", c)
				}
			}
		})
	}
}

func TestHistoryFormOwnershipBoundsAndIgnoresText(t *testing.T) {
	markup := `<script>"<form><input name='dse_sessionId' value='PRIVATE_TOKEN'></form>"</script><!-- <form></form> -->` + strings.Repeat(`<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"></form>`, 12)
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	got := diagnoseHistoryFormOwnership(markup, doc)
	if got.Forms != 12 || got.Controls != 12 || !got.Truncated || len(got.Candidates) != 8 {
		t.Fatalf("incorrect bounded source counts: %+v", got)
	}
	for i, c := range got.Candidates {
		if c.Ordinal != i+1 || c.SourceHasSession {
			t.Fatalf("text treated as markup: %+v", c)
		}
	}
	empty := diagnoseHistoryFormOwnership(markup, nil)
	if empty.Forms != 0 || empty.Controls != 0 || empty.Truncated || len(empty.Candidates) != 0 {
		t.Fatalf("nil DOM invented completeness: %+v", empty)
	}
}
