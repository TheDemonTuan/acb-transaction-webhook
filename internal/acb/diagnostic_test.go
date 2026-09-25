package acb

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosePageStructurePrivacySafe(t *testing.T) {
	markup := `
	<html>
	<head>
		<script>
			var secretCookie = "JSESSIONID=secret123";
			var totalRows = "Tổng số dòng: 999";
		</script>
	</head>
	<body>
		<form action="/acbib/Request?session=sensitive123#frag" method="POST">
			<input type="hidden" name="dse_sessionId" value="opaque-secret-session" />
			<input type="hidden" name="AccountNbr" value="0987654321" />
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
			<input type="hidden" name="dse_processorState" value="initial" />
			<input type="hidden" name="FromDate" value="25/09/2026" />
			<input type="hidden" name="ToDate" value="25/09/2026" />
		</form>
		<div>Tổng số dòng: 2</div>
		<table>
			<tr>
				<th>Ngày hiệu lực</th>
				<th>Ngày giao dịch</th>
				<th>Số GD</th>
				<th>Ghi nợ</th>
				<th>Ghi có</th>
				<th>Số dư</th>
				<th>Nội dung giao dịch</th>
			</tr>
			<tr>
				<td>25/09/2026</td>
				<td>25/09/2026 10:00:00</td>
				<td>TX001</td>
				<td>-</td>
				<td>100.000</td>
				<td>500.000</td>
				<td>Payment from John Doe 0987654321</td>
			</tr>
			<tr>
				<td>25/09/2026</td>
				<td>25/09/2026 11:00:00</td>
				<td>TX002</td>
				<td>50.000</td>
				<td>-</td>
				<td>450.000</td>
				<td>Transfer to Jane Smith</td>
			</tr>
		</table>
	</body>
	</html>
	`

	diag := DiagnosePageStructure(markup)

	if diag.TableCount != 1 {
		t.Errorf("expected TableCount=1, got %d", diag.TableCount)
	}
	if len(diag.HeaderColumnsRecognized) != 7 {
		t.Errorf("expected 7 recognized header columns, got %d: %v", len(diag.HeaderColumnsRecognized), diag.HeaderColumnsRecognized)
	}
	if diag.BodyRowCount != 2 {
		t.Errorf("expected BodyRowCount=2, got %d", diag.BodyRowCount)
	}
	if diag.EmptyMarkerPresent {
		t.Errorf("expected EmptyMarkerPresent=false, got true")
	}
	if !diag.TotalRowsFound || diag.TotalRowsExtracted != 2 {
		t.Errorf("expected TotalRowsExtracted=2, found=true; got %d, %v", diag.TotalRowsExtracted, diag.TotalRowsFound)
	}
	if diag.FormAction != "/acbib/Request" {
		t.Errorf("expected sanitized FormAction=/acbib/Request, got %q", diag.FormAction)
	}

	// Verify form keys are sorted and contain NO values
	expectedKeys := []string{"AccountNbr", "FromDate", "ToDate", "dse_operationName", "dse_processorState", "dse_sessionId"}
	if len(diag.FormKeys) != len(expectedKeys) {
		t.Fatalf("expected %d form keys, got %d: %v", len(expectedKeys), len(diag.FormKeys), diag.FormKeys)
	}
	for i, k := range expectedKeys {
		if diag.FormKeys[i] != k {
			t.Errorf("key mismatch at %d: want %s, got %s", i, k, diag.FormKeys[i])
		}
	}

	// Verify serialized JSON contains NO PII, no account numbers, no cookie values
	rawJSON, err := json.Marshal(diag)
	if err != nil {
		t.Fatalf("json marshal failed: %v", err)
	}
	jsonStr := string(rawJSON)
	piiTokens := []string{
		"0987654321",
		"opaque-secret-session",
		"secret123",
		"sensitive123",
		"John Doe",
		"Jane Smith",
		"999", // From script tag: must NOT be extracted
	}
	for _, tok := range piiTokens {
		if strings.Contains(jsonStr, tok) {
			t.Errorf("diagnostic JSON leaked sensitive token %q: %s", tok, jsonStr)
		}
	}
}

func TestDiagnosePageStructureEmptyState(t *testing.T) {
	markup := `
	<table>
		<tr>
			<th>Số GD</th>
			<th>Ngày giao dịch</th>
			<th>Ghi nợ</th>
			<th>Ghi có</th>
		</tr>
		<tr>
			<td colspan="4">Không có giao dịch</td>
		</tr>
	</table>
	<div>Tổng số dòng: 0</div>
	`

	diag := DiagnosePageStructure(markup)

	if diag.TableCount != 1 {
		t.Errorf("expected TableCount=1, got %d", diag.TableCount)
	}
	if diag.BodyRowCount != 0 {
		t.Errorf("expected BodyRowCount=0, got %d", diag.BodyRowCount)
	}
	if !diag.EmptyMarkerPresent {
		t.Errorf("expected EmptyMarkerPresent=true, got false")
	}
	if !diag.TotalRowsFound || diag.TotalRowsExtracted != 0 {
		t.Errorf("expected TotalRowsExtracted=0, found=true; got %d, %v", diag.TotalRowsExtracted, diag.TotalRowsFound)
	}
}

func TestDiagnosePageStructureIgnoresScriptAndStyle(t *testing.T) {
	markup := `
	<style>
		/* Không có giao dịch */
		.total { content: "Tổng số dòng: 50"; }
	</style>
	<script>
		var msg = "Không có dữ liệu";
		var total = "Tổng số dòng: 100";
	</script>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>1001</td><td>25/09/2026</td><td>0</td><td>10.000</td></tr>
	</table>
	`

	diag := InspectPageStructure(markup)

	if diag.EmptyMarkerPresent {
		t.Errorf("EmptyMarkerPresent should be false when markers are only in script/style tags")
	}
	if diag.TotalRowsFound {
		t.Errorf("TotalRowsFound should be false when total rows is only in script/style tags; got extracted=%d", diag.TotalRowsExtracted)
	}
	if diag.BodyRowCount != 1 {
		t.Errorf("expected BodyRowCount=1, got %d", diag.BodyRowCount)
	}
}
