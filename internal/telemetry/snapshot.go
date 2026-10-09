package telemetry

import "time"

// TelemetrySnapshot represents a bounded, cardinality-controlled operational snapshot.
type TelemetrySnapshot struct {
	CapturedAt       time.Time               `json:"capturedAt"`
	Realtime         RealtimeTelemetry       `json:"realtime"`
	Notifications    NotificationTelemetry   `json:"notifications"`
	MutationGate     MutationGateTelemetry   `json:"mutationGate"`
	Singleton        SingletonTelemetry      `json:"singleton"`
	Deployment       DeploymentTelemetry     `json:"deployment"`
	BackupAndRestore BackupAndDrillTelemetry `json:"backupAndRestore"`
}

type RealtimeTelemetry struct {
	StreamEnabled                    bool    `json:"streamEnabled"`
	StreamState                      string  `json:"streamState"`
	StreamReason                     string  `json:"streamReason,omitempty"`
	StreamStateSince                 string  `json:"streamStateSince,omitempty"`
	StreamConnected                  int64   `json:"streamConnected"`
	StreamReconnectTotal             int64   `json:"streamReconnectTotal"`
	StreamDisconnectTotal            int64   `json:"streamDisconnectTotal"`
	CoordinatorQueueFullTotal        int64   `json:"coordinatorQueueFullTotal"`
	FallbackRecoveryTotal            int64   `json:"fallbackRecoveryTotal"`
	GapRepairTotal                   int64   `json:"gapRepairTotal"`
	P95CommitToGatewayMs             float64 `json:"p95CommitToGatewayMs"`
	P95CommitToBrowserSSEMs          float64 `json:"p95CommitToBrowserSseMs"`
	RecentFallbackRecoveryReconciles int     `json:"recentFallbackRecoveryReconciles"`
	StreamDisconnectedAgeSeconds     float64 `json:"streamDisconnectedAgeSeconds"`
	ConnectedClients                 int64   `json:"connectedClients"`
	P95SSEMs                         float64 `json:"p95SseMs"`
	P95WebhookMs                     float64 `json:"p95WebhookMs"`
	TotalWebhooksSent                int     `json:"totalWebhooksSent"`
}

type NotificationTelemetry struct {
	TotalPending    int                         `json:"totalPending"`
	TotalDeadLetter int                         `json:"totalDeadLetter"`
	ByProvider      map[string]ProviderSnapshot `json:"byProvider"`
	RecentP95Ms     map[string]float64          `json:"recentP95Ms"`
	TotalDelivered  int                         `json:"totalDelivered"`
	TotalFailed     int                         `json:"totalFailed"`
	IsBacklogStuck  bool                        `json:"isBacklogStuck"`
}

type ProviderSnapshot struct {
	Pending    int     `json:"pending"`
	DeadLetter int     `json:"deadLetter"`
	Success    int     `json:"success"`
	Failure    int     `json:"failure"`
	P95Ms      float64 `json:"p95Ms"`
}

type MutationGateTelemetry struct {
	GateState        string  `json:"gateState"`
	Owner            string  `json:"owner,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	ExpiresInSeconds float64 `json:"expiresInSeconds"`
	IsLocked         bool    `json:"isLocked"`
}

type SingletonTelemetry struct {
	WorkerRole          string  `json:"workerRole"`
	WorkerState         string  `json:"workerState"`
	SingletonLockHeld   bool    `json:"singletonLockHeld"`
	MaintenanceOwner    string  `json:"maintenanceOwner"`
	DrainState          string  `json:"drainState"`
	HeartbeatAt         string  `json:"heartbeatAt,omitempty"`
	HeartbeatAgeSeconds float64 `json:"heartbeatAgeSeconds"`
	IsStale             bool    `json:"isStale"`
}

type DeploymentTelemetry struct {
	ActiveSlot          string `json:"activeSlot"`
	ReleaseCommit       string `json:"releaseCommit"`
	RuntimeRole         string `json:"runtimeRole"`
	SchemaVersion       string `json:"schemaVersion"`
	LastPromotionResult string `json:"lastPromotionResult"`
	LastPromotionAt     string `json:"lastPromotionAt,omitempty"`
	FailoverState       string `json:"failoverState"`
}

type BackupAndDrillTelemetry struct {
	LastBackupArtifact      string  `json:"lastBackupArtifact,omitempty"`
	LastBackupAt            string  `json:"lastBackupAt,omitempty"`
	BackupAgeSeconds        float64 `json:"backupAgeSeconds"`
	IsBackupOverdue         bool    `json:"isBackupOverdue"`
	LastRestoreDrillAt      string  `json:"lastRestoreDrillAt,omitempty"`
	LastRestoreDrillSuccess bool    `json:"lastRestoreDrillSuccess"`
	RestoreDrillAgeDays     float64 `json:"restoreDrillAgeDays"`
}
