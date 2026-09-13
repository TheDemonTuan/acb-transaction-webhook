package acb

import (
	"testing"
)

const fixtureHistoryPage1 = `<!DOCTYPE html>
<html>
<body>
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="firstPage" />
		<input type="hidden" name="dse_sessionId" value="ACB_SESS_100" />
		<input type="hidden" name="AccountNbr" value="12345678" />
	</form>
	<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX101</td><td>0</td><td>100.000</td><td>1.000.000</td><td>Transfer 1</td></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX102</td><td>50.000</td><td>0</td><td>950.000</td><td>Payment 2</td></tr>
		<tr><td colspan="7"><a href="/acbib/Request?dse_sessionId=ACB_SESS_100" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
	</table>
	<div>Tổng số dòng: 5</div>
</body>
</html>`

const fixtureHistoryPage2 = `<!DOCTYPE html>
<html>
<body>
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="page2State" />
		<input type="hidden" name="dse_sessionId" value="ACB_SESS_100" />
		<input type="hidden" name="AccountNbr" value="12345678" />
	</form>
	<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX103</td><td>0</td><td>200.000</td><td>1.150.000</td><td>Transfer 3</td></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX104</td><td>0</td><td>300.000</td><td>1.450.000</td><td>Transfer 4</td></tr>
		<tr><td colspan="7"><a href="/acbib/Request?dse_sessionId=ACB_SESS_100" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
	</table>
	<div>Tổng số dòng: 5</div>
</body>
</html>`

const fixtureHistoryPage3Last = `<!DOCTYPE html>
<html>
<body>
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="finalState" />
		<input type="hidden" name="dse_sessionId" value="ACB_SESS_100" />
		<input type="hidden" name="AccountNbr" value="12345678" />
	</form>
	<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX105</td><td>0</td><td>500.000</td><td>1.950.000</td><td>Transfer 5</td></tr>
		<tr><td colspan="7"><span class="disabled">Trang sau</span></td></tr>
	</table>
	<div>Tổng số dòng: 5</div>
</body>
</html>`

const fixtureHistoryTruncated = `<!DOCTYPE html>
<html>
<body>
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="earlyStop" />
		<input type="hidden" name="dse_sessionId" value="ACB_SESS_100" />
	</form>
	<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX201</td><td>0</td><td>10.000</td><td>100.000</td><td>Only Item</td></tr>
		<tr><td colspan="7"><span class="disabled">Trang sau</span></td></tr>
	</table>
	<div>Tổng số dòng: 50</div>
</body>
</html>`

const fixtureHistoryEmptyNavigation = `<!DOCTYPE html>
<html>
<body>
	<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>14/09/2026</td><td>14/09/2026</td><td>TX301</td><td>0</td><td>20.000</td><td>120.000</td><td>Broken Item</td></tr>
		<tr><td colspan="7"><a href="#">Trang sau</a></td></tr>
	</table>
	<div>Tổng số dòng: 10</div>
</body>
</html>`

func TestPaginationCursor_MultiPageFullSync_Fixtures(t *testing.T) {
	cursor := NewPaginationCursor("/acbib/Request", map[string]string{
		"dse_operationName": "ibkacctDetailProc",
		"AccountNbr":        "12345678",
	})

	// Page 1
	p1, err := ParseHistoryPage(fixtureHistoryPage1)
	if err != nil {
		t.Fatalf("ParseHistoryPage page 1 failed: %v", err)
	}
	if !p1.HasNext {
		t.Fatal("expected page 1 to have next page")
	}
	cursor.Step(p1, len(p1.Transactions))

	if cursor.PageNumber != 1 {
		t.Errorf("expected PageNumber 1, got %d", cursor.PageNumber)
	}
	if cursor.CumulativeRows != 2 {
		t.Errorf("expected CumulativeRows 2, got %d", cursor.CumulativeRows)
	}
	if !cursor.HasNext {
		t.Fatal("expected cursor.HasNext to be true after page 1")
	}
	if cursor.Fields["dse_nextEventName"] != "nextPage" {
		t.Errorf("expected nextPage event in fields, got %v", cursor.Fields["dse_nextEventName"])
	}
	if cursor.Fields["dse_processorState"] != "firstPage" {
		t.Errorf("expected firstPage processor state, got %v", cursor.Fields["dse_processorState"])
	}

	// Page 2
	p2, err := ParseHistoryPage(fixtureHistoryPage2)
	if err != nil {
		t.Fatalf("ParseHistoryPage page 2 failed: %v", err)
	}
	if !p2.HasNext {
		t.Fatal("expected page 2 to have next page")
	}
	cursor.Step(p2, len(p2.Transactions))

	if cursor.PageNumber != 2 {
		t.Errorf("expected PageNumber 2, got %d", cursor.PageNumber)
	}
	if cursor.CumulativeRows != 4 {
		t.Errorf("expected CumulativeRows 4, got %d", cursor.CumulativeRows)
	}
	if !cursor.HasNext {
		t.Fatal("expected cursor.HasNext to be true after page 2")
	}
	if cursor.Fields["dse_processorState"] != "page2State" {
		t.Errorf("expected page2State, got %v", cursor.Fields["dse_processorState"])
	}

	// Page 3 (Final)
	p3, err := ParseHistoryPage(fixtureHistoryPage3Last)
	if err != nil {
		t.Fatalf("ParseHistoryPage page 3 failed: %v", err)
	}
	if p3.HasNext {
		t.Fatal("expected page 3 to NOT have next page")
	}
	cursor.Step(p3, len(p3.Transactions))

	if cursor.PageNumber != 3 {
		t.Errorf("expected PageNumber 3, got %d", cursor.PageNumber)
	}
	if cursor.CumulativeRows != 5 {
		t.Errorf("expected CumulativeRows 5, got %d", cursor.CumulativeRows)
	}
	if cursor.HasNext {
		t.Fatal("expected cursor.HasNext to be false after last page")
	}
	if !cursor.Complete {
		t.Fatal("expected cursor.Complete to be true")
	}
	if cursor.Truncated {
		t.Fatal("expected cursor.Truncated to be false")
	}
}

func TestPaginationCursor_TruncationDetection(t *testing.T) {
	cursor := NewPaginationCursor("/acbib/Request", map[string]string{
		"dse_operationName": "ibkacctDetailProc",
	})

	p, err := ParseHistoryPage(fixtureHistoryTruncated)
	if err != nil {
		t.Fatalf("ParseHistoryPage failed: %v", err)
	}
	cursor.Step(p, len(p.Transactions))

	if cursor.HasNext {
		t.Fatal("expected HasNext to be false")
	}
	if cursor.Complete {
		t.Fatal("expected Complete to be false due to truncation")
	}
	if !cursor.Truncated {
		t.Fatal("expected Truncated to be true (parsed 1 of 50 total rows)")
	}
}

func TestPaginationCursor_MalformedNavigationStop(t *testing.T) {
	cursor := NewPaginationCursor("/acbib/Request", map[string]string{
		"dse_operationName": "ibkacctDetailProc",
	})

	p, err := ParseHistoryPage(fixtureHistoryEmptyNavigation)
	if err != nil {
		t.Fatalf("ParseHistoryPage failed: %v", err)
	}
	// HasNext should be false since href is "#" and no onclick
	cursor.Step(p, len(p.Transactions))

	if cursor.HasNext {
		t.Fatal("expected HasNext to be false for anchor with href=# and no onclick")
	}
	if cursor.Complete {
		t.Fatal("expected Complete to be false (total 10, cumulative 1)")
	}
	if !cursor.Truncated {
		t.Fatal("expected Truncated to be true")
	}
}

func TestPaginationCursor_StateIsolation(t *testing.T) {
	// Proves that two concurrent cursors (e.g. history and realtime) keep isolated form states
	c1 := NewPaginationCursor("/history", map[string]string{"FromDate": "01/08/2026", "ToDate": "10/08/2026"})
	c2 := NewPaginationCursor("/realtime", map[string]string{"FromDate": "14/09/2026", "ToDate": "14/09/2026"})

	p1, _ := ParseHistoryPage(fixtureHistoryPage1)
	c1.Step(p1, len(p1.Transactions))

	// Verify c1 was updated but c2 remains completely untouched
	if c1.Fields["dse_processorState"] != "firstPage" {
		t.Errorf("c1 expected firstPage, got %v", c1.Fields["dse_processorState"])
	}
	if c2.Fields["dse_processorState"] != "" {
		t.Errorf("c2 should not have processorState, got %v", c2.Fields["dse_processorState"])
	}
	if c2.Fields["FromDate"] != "14/09/2026" {
		t.Errorf("c2 FromDate corrupted: %v", c2.Fields["FromDate"])
	}
}
