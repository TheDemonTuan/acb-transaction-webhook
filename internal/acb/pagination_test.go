package acb

import (
	"testing"
)

func TestParseHistoryPageActiveNextLink(t *testing.T) {
	html := `<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="next" />
		<input type="hidden" name="dse_sessionId" value="session123" />
	</form>
	<table>
		<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>12/09/2026</td><td>TX1001</td><td>0</td><td>50.000</td></tr>
		<tr><td colspan="4"><a href="/acbib/Request?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
	</table>`

	res, err := ParseHistoryPage(html)
	if err != nil {
		t.Fatalf("ParseHistoryPage failed: %v", err)
	}
	if len(res.Transactions) != 1 || res.Transactions[0].Number != "TX1001" {
		t.Fatalf("unexpected transactions: %+v", res.Transactions)
	}
	if !res.HasNext {
		t.Fatal("expected HasNext to be true for active Trang sau link")
	}
	if res.NextFields == nil || res.NextFields["dse_nextEventName"] != "nextPage" {
		t.Fatalf("expected NextFields with dse_nextEventName=nextPage, got %+v", res.NextFields)
	}
	if res.NextFields["_raw"] != "true" {
		t.Fatalf("expected _raw=true in NextFields, got %+v", res.NextFields)
	}
}

func TestParseHistoryPageDisabledNextLink(t *testing.T) {
	html := `<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="last" />
	</form>
	<table>
		<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>12/09/2026</td><td>TX1002</td><td>0</td><td>150.000</td></tr>
		<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
	</table>`

	res, err := ParseHistoryPage(html)
	if err != nil {
		t.Fatalf("ParseHistoryPage failed: %v", err)
	}
	if len(res.Transactions) != 1 || res.Transactions[0].Number != "TX1002" {
		t.Fatalf("unexpected transactions: %+v", res.Transactions)
	}
	if res.HasNext {
		t.Fatal("expected HasNext to be false for disabled next link")
	}
}

func TestParseHistoryPageTruncationDetection(t *testing.T) {
	html := `<table>
		<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>12/09/2026</td><td>TX1003</td><td>0</td><td>250.000</td></tr>
	</table>
	<div>Tổng số dòng: 100</div>`

	res, err := ParseHistoryPage(html)
	if err != nil {
		t.Fatalf("ParseHistoryPage failed: %v", err)
	}
	if res.TotalRows != 100 {
		t.Fatalf("expected TotalRows=100, got %d", res.TotalRows)
	}
	if res.HasNext {
		t.Fatal("expected HasNext=false when no next page link is present")
	}
}

func TestParseHistoryPageNextDisabledAttributeAndName(t *testing.T) {
	t.Run("input button with name NEXT_DISABLED is recognized as disabled", func(t *testing.T) {
		html := `<form action="/acbib/Request" method="POST">
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		</form>
		<table>
			<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
			<tr><td>12/09/2026</td><td>TX2001</td><td>0</td><td>10.000</td></tr>
			<tr><td colspan="4"><input type="button" name="NEXT_DISABLED" value="Trang sau" onclick="submitEvent('nextPage')" /></td></tr>
		</table>`
		res, err := ParseHistoryPage(html)
		if err != nil {
			t.Fatal(err)
		}
		if res.HasNext {
			t.Fatal("expected HasNext to be false when next button has name NEXT_DISABLED")
		}
	})

	t.Run("hidden input with name NEXT_DISABLED on page disables next", func(t *testing.T) {
		html := `<form action="/acbib/Request" method="POST">
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
			<input type="hidden" name="NEXT_DISABLED" value="true" />
		</form>
		<table>
			<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
			<tr><td>12/09/2026</td><td>TX2002</td><td>0</td><td>20.000</td></tr>
			<tr><td colspan="4"><a href="/next" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`
		res, err := ParseHistoryPage(html)
		if err != nil {
			t.Fatal(err)
		}
		if res.HasNext {
			t.Fatal("expected HasNext to be false when page contains NEXT_DISABLED input")
		}
	})
}
