package telemetry

import (
	"testing"
	"time"
)

func TestTelemetry_BackupOverdueAndDrillAlerts(t *testing.T) {
	reg := NewRegistry()

	t.Run("fresh backup produces no overdue alert", func(t *testing.T) {
		reg.SetBackup("gateway-20260914100000.db.age", time.Now().UTC().Add(-2*time.Hour))
		reg.SetRestoreDrill(time.Now().UTC().Add(-5*24*time.Hour), true)

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var bkpAlert, drillAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertBackupOverdue {
				bkpAlert = &a
			}
			if a.Name == AlertRestoreDrillOverdue {
				drillAlert = &a
			}
		}

		if bkpAlert == nil || bkpAlert.Active {
			t.Errorf("expected backup alert inactive for 2h old backup, got active: %v", bkpAlert)
		}
		if drillAlert == nil || drillAlert.Active {
			t.Errorf("expected drill alert inactive for 5d old drill, got active: %v", drillAlert)
		}
	})

	t.Run("backup older than 24 hours triggers ALERT_BACKUP_OVERDUE", func(t *testing.T) {
		reg.SetBackup("gateway-20260913000000.db.age", time.Now().UTC().Add(-26*time.Hour))

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var bkpAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertBackupOverdue {
				bkpAlert = &a
				break
			}
		}

		if bkpAlert == nil || !bkpAlert.Active {
			t.Fatalf("expected ALERT_BACKUP_OVERDUE active for 26h old backup")
		}
		if bkpAlert.Level != AlertWarning {
			t.Errorf("expected WARNING level, got %v", bkpAlert.Level)
		}
	})

	t.Run("restore drill older than 30 days triggers ALERT_RESTORE_DRILL_OVERDUE", func(t *testing.T) {
		reg.SetRestoreDrill(time.Now().UTC().Add(-35*24*time.Hour), true)

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var drillAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertRestoreDrillOverdue {
				drillAlert = &a
				break
			}
		}

		if drillAlert == nil || !drillAlert.Active {
			t.Fatalf("expected ALERT_RESTORE_DRILL_OVERDUE active for 35d old drill")
		}
		if drillAlert.Level != AlertWarning {
			t.Errorf("expected WARNING level, got %v", drillAlert.Level)
		}
	})

	t.Run("failed restore drill triggers CRITICAL alert", func(t *testing.T) {
		reg.SetRestoreDrill(time.Now().UTC().Add(-1*time.Hour), false)

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var drillAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertRestoreDrillOverdue {
				drillAlert = &a
				break
			}
		}

		if drillAlert == nil || !drillAlert.Active {
			t.Fatalf("expected ALERT_RESTORE_DRILL_OVERDUE active for failed drill")
		}
		if drillAlert.Level != AlertCritical {
			t.Errorf("expected CRITICAL level for failed drill, got %v", drillAlert.Level)
		}
	})
}
