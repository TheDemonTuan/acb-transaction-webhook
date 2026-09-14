package telemetry

import "time"

// TelemetrySnapshot represents a bounded, cardinality-controlled operational snapshot.
type TelemetrySnapshot struct {
	CapturedAt       time.Time               `json:"capturedAt"`
	Scheduler        SchedulerTelemetry      `json:"scheduler"`
	Realtime         RealtimeTelemetry       `json:"realtime"`
	HistoryJobs      HistoryJobsTelemetry    `json:"historyJobs"`
	AuthLifecycle    AuthLifecycleTelemetry  `json:"authLifecycle"`
	Notifications    NotificationTelemetry   `json:"notifications"`
	MutationGate     MutationGateTelemetry   `json:"mutationGate"`
	Singleton        SingletonTelemetry      `json:"singleton"`
	Deployment       DeploymentTelemetry     `json:"deployment"`
	BackupAndRestore BackupAndDrillTelemetry `json:"backupAndRestore"`
}

type SchedulerTelemetry struct {
	QueueDepthByPriority  map[string]int     `json:"queueDepthByPriority"`
	TotalQueueDepth       int                `json:"totalQueueDepth"`
	CurrentTaskKind       string             `json:"currentTaskKind"`
	CurrentTaskDurationMs float64            `json:"currentTaskDurationMs"`
	IsBusy                bool               `json:"isBusy"`
	Enqueued              map[string]int64   `json:"enqueued"`
	Started               map[string]int64   `json:"started"`
	Completed             map[string]int64   `json:"completed"`
	Yielded               map[string]int64   `json:"yielded"`
	Failed                map[string]int64   `json:"failed"`
	Overloaded            int64              `json:"overloaded"`
	P95LatencyMs          map[string]float64 `json:"p95LatencyMs"`
}

type RealtimeTelemetry struct {
	LastACBPollAt         string  `json:"lastAcbPollAt,omitempty"`
	LastACBPollAgeSeconds float64 `json:"lastAcbPollAgeSeconds"`
	LastACBPollDurationMs float64 `json:"lastAcbPollDurationMs"`
	LastACBPollStatus     string  `json:"lastAcbPollStatus"`
	CatchUpDay            string  `json:"catchUpDay,omitempty"`
	CircuitBreakerOpen    bool    `json:"circuitBreakerOpen"`
	ConnectedClients      int64   `json:"connectedClients"`
	P95IngestMs           float64 `json:"p95IngestMs"`
	P95SSEMs              float64 `json:"p95SseMs"`
	P95WebhookMs          float64 `json:"p95WebhookMs"`
	TotalIngested         int     `json:"totalIngested"`
	TotalWebhooksSent     int     `json:"totalWebhooksSent"`
}

type HistoryJobsTelemetry struct {
	CountsByStatus         map[string]int `json:"countsByStatus"`
	TotalJobs              int            `json:"totalJobs"`
	OldestQueuedAgeSeconds float64        `json:"oldestQueuedAgeSeconds"`
	StalledCount           int            `json:"stalledCount"`
	TotalPagesDone         int            `json:"totalPagesDone"`
	TotalRowsSeen          int            `json:"totalRowsSeen"`
}

type AuthLifecycleTelemetry struct {
	HasActiveAttempt        bool           `json:"hasActiveAttempt"`
	ActiveAttemptAgeSeconds float64        `json:"activeAttemptAgeSeconds"`
	ActiveAttemptStuck      bool           `json:"activeAttemptStuck"`
	SessionState            string         `json:"sessionState"`
	RecentAttemptsCount     int            `json:"recentAttemptsCount"`
	AttemptsByStatus        map[string]int `json:"attemptsByStatus"`
}

type NotificationTelemetry struct {
	TotalPending      int                         `json:"totalPending"`
	TotalDeadLetter   int                         `json:"totalDeadLetter"`
	ByProvider        map[string]ProviderSnapshot `json:"byProvider"`
	RecentP95Ms       map[string]float64          `json:"recentP95Ms"`
	TotalDelivered    int                         `json:"totalDelivered"`
	TotalFailed       int                         `json:"totalFailed"`
	IsBacklogStuck    bool                        `json:"isBacklogStuck"`
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
	LastBackupArtifact     string  `json:"lastBackupArtifact,omitempty"`
	LastBackupAt           string  `json:"lastBackupAt,omitempty"`
	BackupAgeSeconds       float64 `json:"backupAgeSeconds"`
	IsBackupOverdue        bool    `json:"isBackupOverdue"`
	LastRestoreDrillAt     string  `json:"lastRestoreDrillAt,omitempty"`
	LastRestoreDrillSuccess bool   `json:"lastRestoreDrillSuccess"`
	RestoreDrillAgeDays    float64 `json:"restoreDrillAgeDays"`
}
