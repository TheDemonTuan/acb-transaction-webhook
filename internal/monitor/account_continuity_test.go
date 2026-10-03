package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestHistoryAccountOmissionNeverQueriesMaskedAccount(t *testing.T) {
	for _, consumer := range []string{"realtime", "catchup", "history_job"} {
		for _, emptyField := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", consumer, emptyField), func(t *testing.T) {
				ctx := context.Background()
				store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "account.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				conn, err := store.ConfigureConnection(ctx, "***2222")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
					t.Fatal(err)
				}
				calls := 0
				client, err := acb.NewClient("https://online.acb.com.vn", verifierAccountTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodPost || r.ParseForm() != nil {
						t.Fatal("history must issue a valid POST")
					}
					if r.PostForm.Get("AccountNbr") != "222222222" {
						t.Fatal("missing bank echo replaced exact selection with masked/empty request")
					}
					calls++
					account := ""
					if emptyField {
						account = `<input name="AccountNbr" value="">`
					}
					body := fmt.Sprintf(`<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-%d">%s</form><table><tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td colspan="4">Không có giao dịch</td></tr></table>`, calls, account)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseIdleConnections()
				if err := client.RestoreSession(authbrowser.Handoff{Version: 1, Action: "https://online.acb.com.vn/acbib/Request", Fields: map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_processorState": "acctDetailPage", "dse_sessionId": "initial", "AccountNbr": "222222222"}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: acb.OfficialHost, Path: "/", Secure: true}}}); err != nil {
					t.Fatal(err)
				}
				mon := New(store, client, 5*time.Second, 15*time.Second)
				fixed := time.Now().In(acb.DefaultLocation)
				mon.now = func() time.Time { return fixed }
				var task interface {
					Step(context.Context) (scheduler.TaskStepResult, error)
				}
				switch consumer {
				case "realtime":
					task = NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
				case "catchup":
					catchup := newTestCatchUpTask(mon, conn.ID, conn.Generation)
					catchup.fromDate, catchup.toDate = fixed.Format("2006-01-02"), fixed.Format("2006-01-02")
					catchup.currentDay, _ = time.Parse("2006-01-02", catchup.fromDate)
					catchup.initialized = true
					task = catchup
				case "history_job":
					day := fixed.Format("2006-01-02")
					if _, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, day, day); err != nil {
						t.Fatal(err)
					}
					job, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now())
					if err != nil || !claimed {
						t.Fatalf("claim history job: %v", err)
					}
					task = NewHistoryJobTask(NewHistoryJobRunner(store, client, nil, nil), job, conn)
				}
				done := false
				for i := 0; i < 5 && !done; i++ {
					result, err := task.Step(ctx)
					if err != nil {
						t.Fatal(err)
					}
					done = result.Done
				}
				if !done {
					t.Fatal("history consumer failed to complete the supported empty day")
				}
				snapshot, err := client.SnapshotSession()
				if err != nil || snapshot.Fields["AccountNbr"] != "222222222" || snapshot.Fields["dse_sessionId"] != fmt.Sprintf("fresh-%d", calls) {
					t.Fatal("completed history lost exact selection or fresh conversational state", err)
				}
				if consumer == "realtime" {
					runs, err := store.ListPollRuns(ctx, 1)
					if err != nil || len(runs) != 1 || runs[0].Status != "EMPTY_OK" && runs[0].Status != "SUCCEEDED" {
						t.Fatalf("realtime did not commit empty history outcome: %+v %v", runs, err)
					}
				} else {
					covered, err := store.CheckRangeCoverage(ctx, conn.ID, fixed.Format("2006-01-02"), fixed.Format("2006-01-02"))
					if err != nil || !covered {
						t.Fatal("completed history did not commit complete day coverage", err)
					}
				}
			})
		}
	}
}
