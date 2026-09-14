package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InspectBackupAndDrill checks local filesystem artifacts for backup age and restore drill records.
func InspectBackupAndDrill(backupDir string, evidencePaths ...string) BackupAndDrillTelemetry {
	res := BackupAndDrillTelemetry{
		IsBackupOverdue: true,
	}

	// 1. Scan backup files (*.db.age)
	if backupDir != "" {
		if entries, err := os.ReadDir(backupDir); err == nil {
			type fileInfo struct {
				name    string
				modTime time.Time
			}
			var backups []fileInfo
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if strings.HasSuffix(name, ".db.age") {
					if info, err := e.Info(); err == nil {
						backups = append(backups, fileInfo{
							name:    name,
							modTime: info.ModTime().UTC(),
						})
					}
				}
			}
			if len(backups) > 0 {
				sort.Slice(backups, func(i, j int) bool {
					return backups[i].modTime.After(backups[j].modTime)
				})
				latest := backups[0]
				res.LastBackupArtifact = latest.name
				res.LastBackupAt = latest.modTime.Format(time.RFC3339)
				res.BackupAgeSeconds = time.Since(latest.modTime).Seconds()
				if res.BackupAgeSeconds < 0 {
					res.BackupAgeSeconds = 0
				}
				res.IsBackupOverdue = (res.BackupAgeSeconds > 86400)
			}
		}
	}

	// 2. Scan restore drill evidence
	type drillEvidence struct {
		DrillID   string `json:"drill_id"`
		Timestamp string `json:"timestamp"`
		Status    string `json:"status"`
		Success   bool   `json:"success"`
	}

	for _, p := range evidencePaths {
		if p == "" {
			continue
		}
		if data, err := os.ReadFile(p); err == nil {
			var ev drillEvidence
			if err := json.Unmarshal(data, &ev); err == nil {
				res.LastRestoreDrillAt = ev.Timestamp
				res.LastRestoreDrillSuccess = ev.Success || (ev.Status == "SUCCESS")
				if t, err := time.Parse(time.RFC3339, ev.Timestamp); err == nil {
					res.RestoreDrillAgeDays = time.Since(t).Hours() / 24.0
				}
				break
			}
		}
	}

	return res
}

// InspectFailoverState reads VPS failover controller state from disk if available.
func InspectFailoverState(stateDir, app string) string {
	if stateDir == "" {
		stateDir = "/var/lib/vps-failover/apps"
	}
	if app == "" {
		app = "acb"
	}
	p := filepath.Join(stateDir, app, "state.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return "UNKNOWN"
	}
	var st struct {
		Role           string `json:"role"`
		State          string `json:"state"`
		DegradedReason string `json:"degraded_reason"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "CORRUPTED"
	}
	if st.DegradedReason != "" {
		return "DEGRADED"
	}
	if st.Role != "" {
		return strings.ToUpper(st.Role)
	}
	if st.State != "" {
		return strings.ToUpper(st.State)
	}
	return "UNKNOWN"
}
