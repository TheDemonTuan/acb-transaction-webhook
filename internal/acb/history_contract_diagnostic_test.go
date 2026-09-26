package acb

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestHistoryContractDiagnosticRedactsAndBounds(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	client := &Client{}
	fields := map[string]string{"FromDate": "26/09/2026", "ToDate": "26/09/2026", "activeDatetimeYN": "N", "AccountNbr": "PRIVATE_ACCOUNT", "dse_sessionId": "PRIVATE_SESSION"}
	response := Response{StatusCode: 200, Kind: HistoryPage, Body: `<form action="/acbib/Request?token=PRIVATE_TOKEN"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="PRIVATE_STATE"><input name="FromDate" value="PRIVATE_DATE"><input name="activeDatetimeYN" value="PRIVATE_SELECTOR"></form><table><tr><th>Ngày giao dịch</th><th>Ngày hiệu lực</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Nội dung giao dịch</th></tr><tr><td>26/09/2026</td><td>26/09/2026</td><td>PRIVATE_ID</td><td>123456789</td><td>0</td><td>PRIVATE_DESCRIPTION</td></tr></table>`}
	for i := 0; i < 20; i++ {
		client.logHistoryContract(fields, response)
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
