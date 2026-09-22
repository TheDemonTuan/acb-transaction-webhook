package acb

import (
	"testing"
)

func TestNormalized(t *testing.T) {
	for _, h := range []string{"Ngày hiệu lực", "Ngày giao dịch", "Số GD", "Ghi nợ", "Ghi có", "Số dư"} {
		t.Logf("%q -> %q", h, normalized(h))
	}
}

func TestParseHistory(t *testing.T) {
	transactions, err := ParseHistory(`<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr><tr><td>10/09/2026</td><td>10/09/2026</td><td>2629</td><td>-</td><td>50.000</td><td>1.250.000</td><td>Thanh toan</td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 1 || transactions[0].Credit != 50000 || transactions[0].Balance == nil || *transactions[0].Balance != 1250000 {
		t.Fatalf("transactions=%+v", transactions)
	}
}

func TestParseHistorySkipsHeaderOnlyTableBeforeData(t *testing.T) {
	html := `<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr></table>
	<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
	<tr><td>12/09/2026</td><td>123</td><td>-</td><td>50.000</td></tr></table>`
	transactions, err := ParseHistory(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 1 || transactions[0].Number != "123" {
		t.Fatalf("transactions=%+v", transactions)
	}
}

func TestParseHistoryRejectsHeaderOnlyWithoutEmptyMarker(t *testing.T) {
	_, err := ParseHistory(`<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr></table>`)
	if err == nil {
		t.Fatal("accepted ambiguous header-only history as empty")
	}
}

func TestParseHistorySkipsFooterBeforeRecognizedEmptyMarker(t *testing.T) {
	transactions, err := ParseHistory(`<table>
	<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
	<tr><td colspan="4">Trang trước | Trang sau | Xuất Excel</td></tr>
	<tr><td colspan="4">Không có giao dịch</td></tr>
	</table>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 0 {
		t.Fatalf("transactions=%+v", transactions)
	}
}

func TestParseHistoryNestedTablesWithOuterSummary(t *testing.T) {
	html := `
	<table id="outer">
		<tr><td><table id="account"><tr><td>Account info</td></tr></table></td></tr>
		<tr><td>
			<table id="history">
				<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th></tr>
				<tr><td>12/09/2026</td><td>12/09/2026 10:00:00</td><td>1001</td><td></td><td>50.000</td><td>500.000</td></tr>
				<tr><td></td><td colspan="5">Chuyen tien thanh toan</td><td></td></tr>
				<tr><td>12/09/2026</td><td>12/09/2026 11:00:00</td><td>1002</td><td>20.000</td><td></td><td>480.000</td></tr>
				<tr><td></td><td colspan="5">Rut tien</td><td></td></tr>
			</table>
		</td></tr>
		<tr><td>Tổng rút ra: 20.000 (VND)  Tổng gửi vào: 50.000 (VND)</td></tr>
		<tr><td>Số dư cuối: 480.000 (VND)</td></tr>
	</table>`

	transactions, err := ParseHistory(html)
	if err != nil {
		t.Fatalf("ParseHistory failed: %v", err)
	}
	if len(transactions) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(transactions))
	}
	if transactions[0].Number != "1001" || transactions[0].Credit != 50000 || transactions[0].Description != "Chuyen tien thanh toan" {
		t.Fatalf("unexpected txn 0: %+v", transactions[0])
	}
	if transactions[1].Number != "1002" || transactions[1].Debit != 20000 || transactions[1].Description != "Rut tien" {
		t.Fatalf("unexpected txn 1: %+v", transactions[1])
	}
}

func TestParseHistoryAcceptsRecognizedEmptyMarker(t *testing.T) {
	transactions, err := ParseHistory(`<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 0 {
		t.Fatalf("transactions=%+v", transactions)
	}
}

func TestParseHistoryAcceptsNoDataMarker(t *testing.T) {
	transactions, err := ParseHistory(`<table>
	<tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>
	<tr><td colspan="4" class="text-center">Không có dữ liệu trong khoảng thời gian đã chọn</td></tr>
	</table>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 0 {
		t.Fatalf("transactions=%+v", transactions)
	}
}

func TestParseHistoryACBTwoRowLayout(t *testing.T) {
	html := `<table>
		<tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th></tr>
		<tr><td>20/08/2026</td><td>20/08/2026 17:09:22</td><td>2612</td><td></td><td>50.000</td><td>50.000</td></tr>
		<tr><td></td><td colspan="5">RUT TIEN TU VI MOMO 0844343536 CASHOUT</td><td></td></tr>
		<tr><td>20/08/2026</td><td>20/08/2026 17:15:54</td><td>2614</td><td>100.000</td><td></td><td></td></tr>
		<tr><td></td><td colspan="5">NAP TIEN VAO VI MOMO</td><td></td></tr>
	</table>`
	transactions, err := ParseHistory(html)
	if err != nil {
		t.Fatalf("ParseHistory failed: %v", err)
	}
	if len(transactions) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(transactions))
	}
	if transactions[0].Number != "2612" || transactions[0].Credit != 50000 || transactions[0].Description != "RUT TIEN TU VI MOMO 0844343536 CASHOUT" {
		t.Fatalf("unexpected txn 0: %+v", transactions[0])
	}
	if transactions[1].Number != "2614" || transactions[1].Debit != 100000 || transactions[1].Description != "NAP TIEN VAO VI MOMO" {
		t.Fatalf("unexpected txn 1: %+v", transactions[1])
	}
}

func TestParseHistoryRejectsUnknownSchema(t *testing.T) {
	if _, err := ParseHistory(`<table><tr><th>not history</th></tr></table>`); err == nil {
		t.Fatal("accepted unknown table")
	}
}

func TestParseHistoryRejectsInvalidMoney(t *testing.T) {
	_, err := ParseHistory(`<table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td>10/09</td><td>1</td><td>-</td><td>50K</td></tr></table>`)
	if err == nil {
		t.Fatal("accepted invalid money")
	}
}
