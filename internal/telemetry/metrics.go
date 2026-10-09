package telemetry

import (
	"sort"
	"sync"
	"time"
)

type RealtimeMetricsReport struct {
	ConnectedClients  int64                         `json:"connectedClients"`
	P95SSEMs          float64                       `json:"p95SseMs"`
	P95WebhookMs      float64                       `json:"p95WebhookMs"`
	TotalWebhooksSent int                           `json:"totalWebhooksSent"`
	Notifications     map[string]NotificationMetric `json:"notifications"`
}

type NotificationMetric struct {
	Success int     `json:"success"`
	Failure int     `json:"failure"`
	P95Ms   float64 `json:"p95Ms"`
}

type Registry struct {
	mu sync.RWMutex

	// Realtime & HTTP samples
	sseSamples           []float64
	webhookSamples       []float64
	notificationSamples  map[string][]float64
	notificationTotals   map[string]map[string]int
	connectedClients     int64
	totalWebhooksSent    int
	streamEnabled        bool
	streamState          string
	streamReason         string
	streamStateSince     time.Time
	streamReconnects     int64
	streamDisconnects    int64
	coordinatorQueueFull int64
	fallbackRecoveries   int64
	gapRepairs           int64
	fallbackTimes        []time.Time
	commitGatewaySamples []float64
	commitBrowserSamples []float64

	// Notification backlog telemetry
	notifTotalPending      int
	notifTotalDeadLetter   int
	notifBacklogByProvider map[string]ProviderSnapshot
	notifBacklogStuck      bool

	// Mutation gate telemetry
	gateState     string
	gateOwner     string
	gateReason    string
	gateExpiresIn time.Duration
	gateIsLocked  bool

	// Singleton worker telemetry
	workerRole           string
	workerState          string
	singletonLock        bool
	drainState           string
	workerHeartbeat      time.Time
	workerStaleThreshold time.Duration

	// Deployment & Failover telemetry
	activeSlot          string
	releaseCommit       string
	runtimeRole         string
	schemaVersion       string
	lastPromotionResult string
	lastPromotionAt     time.Time
	failoverState       string

	// Backup & Restore drill telemetry
	backupArtifact      string
	backupAt            time.Time
	backupAge           time.Duration
	backupOverdue       bool
	restoreDrillAt      time.Time
	restoreDrillSuccess bool
	restoreDrillAgeDays float64
}

var Default = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{
		sseSamples:             make([]float64, 0, 1000),
		webhookSamples:         make([]float64, 0, 1000),
		notificationSamples:    make(map[string][]float64),
		notificationTotals:     make(map[string]map[string]int),
		notifBacklogByProvider: make(map[string]ProviderSnapshot),
		workerState:            "READY",
		workerStaleThreshold:   60 * time.Second,
		failoverState:          "PRIMARY",
		streamState:            "disabled",
		commitGatewaySamples:   make([]float64, 0, 1000),
		commitBrowserSamples:   make([]float64, 0, 1000),
		fallbackTimes:          make([]time.Time, 0, 64),
	}
}

func (r *Registry) RecordSSE(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sseSamples) >= 1000 {
		r.sseSamples = r.sseSamples[1:]
	}
	r.sseSamples = append(r.sseSamples, ms)
}

func (r *Registry) RecordWebhook(d time.Duration, delivered ...bool) {
	ms := float64(d.Microseconds()) / 1000.0
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(delivered) == 0 || delivered[0] {
		r.totalWebhooksSent++
	}
	if len(r.webhookSamples) >= 1000 {
		r.webhookSamples = r.webhookSamples[1:]
	}
	r.webhookSamples = append(r.webhookSamples, ms)
}

func (r *Registry) RecordNotification(provider string, d time.Duration, success bool) {
	outcome := "failure"
	if success {
		outcome = "success"
	}
	ms := float64(d.Microseconds()) / 1000.0
	r.mu.Lock()
	defer r.mu.Unlock()
	samples := r.notificationSamples[provider]
	if len(samples) >= 1000 {
		samples = samples[1:]
	}
	r.notificationSamples[provider] = append(samples, ms)
	if r.notificationTotals[provider] == nil {
		r.notificationTotals[provider] = make(map[string]int)
	}
	r.notificationTotals[provider][outcome]++
}

func (r *Registry) SetConnectedClients(count int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectedClients = count
}

func (r *Registry) SetRealtimeStreamState(enabled bool, state, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !enabled {
		state = "disabled"
		reason = ""
	}
	if r.streamEnabled == enabled && r.streamState == state && r.streamReason == reason {
		return
	}
	r.streamEnabled = enabled
	r.streamState = state
	r.streamReason = reason
	r.streamStateSince = time.Now().UTC()
}

func (r *Registry) RecordRealtimeReconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamReconnects++
}

func (r *Registry) RecordRealtimeDisconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamDisconnects++
}

func (r *Registry) RecordRealtimeCoordinatorQueueFull() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.coordinatorQueueFull++
}

func (r *Registry) RecordFallbackRecovery(count int, gap bool) {
	if count <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallbackRecoveries += int64(count)
	if gap {
		r.gapRepairs += int64(count)
	}
	now := time.Now().UTC()
	r.fallbackTimes = append(r.fallbackTimes, now)
	cutoff := now.Add(-time.Minute)
	first := 0
	for first < len(r.fallbackTimes) && r.fallbackTimes[first].Before(cutoff) {
		first++
	}
	if first > 0 {
		r.fallbackTimes = append([]time.Time(nil), r.fallbackTimes[first:]...)
	}
}

func (r *Registry) RecordCommitToGateway(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.commitGatewaySamples) >= 1000 {
		r.commitGatewaySamples = r.commitGatewaySamples[1:]
	}
	r.commitGatewaySamples = append(r.commitGatewaySamples, float64(d.Microseconds())/1000.0)
}

func (r *Registry) RecordCommitToBrowserSSE(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.commitBrowserSamples) >= 1000 {
		r.commitBrowserSamples = r.commitBrowserSamples[1:]
	}
	r.commitBrowserSamples = append(r.commitBrowserSamples, float64(d.Microseconds())/1000.0)
}

// Notification backlog telemetry

func (r *Registry) SetNotificationBacklog(pending, deadLetter int, stuck bool, byProvider map[string]ProviderSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifTotalPending = pending
	r.notifTotalDeadLetter = deadLetter
	r.notifBacklogStuck = stuck
	r.notifBacklogByProvider = make(map[string]ProviderSnapshot, len(byProvider))
	for k, v := range byProvider {
		r.notifBacklogByProvider[k] = v
	}
}

// Mutation gate telemetry

func (r *Registry) SetMutationGate(gateState, owner, reason string, expiresIn time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gateState = gateState
	r.gateOwner = owner
	r.gateReason = reason
	r.gateExpiresIn = expiresIn
	r.gateIsLocked = (gateState == "LOCKED")
}

// Singleton worker telemetry

func (r *Registry) SetWorkerSingleton(role, state string, lockHeld bool, drainState string, lastHb time.Time, staleThreshold time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workerRole = role
	r.workerState = state
	r.singletonLock = lockHeld
	r.drainState = drainState
	r.workerHeartbeat = lastHb
	if staleThreshold > 0 {
		r.workerStaleThreshold = staleThreshold
	}
}

// Deployment & Failover telemetry

func (r *Registry) SetDeployment(slot, release, role, schema string, lastPromotionResult string, lastPromotionAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activeSlot = slot
	r.releaseCommit = release
	r.runtimeRole = role
	r.schemaVersion = schema
	r.lastPromotionResult = lastPromotionResult
	r.lastPromotionAt = lastPromotionAt
}

func (r *Registry) SetFailover(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failoverState = state
}

// Backup & Restore drill telemetry

func (r *Registry) SetBackup(artifact string, backupAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backupArtifact = artifact
	r.backupAt = backupAt
	if !backupAt.IsZero() {
		r.backupAge = time.Since(backupAt)
		r.backupOverdue = (r.backupAge > 24*time.Hour)
	} else {
		r.backupOverdue = true
	}
}

func (r *Registry) SetRestoreDrill(drillAt time.Time, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restoreDrillAt = drillAt
	r.restoreDrillSuccess = success
	if !drillAt.IsZero() {
		r.restoreDrillAgeDays = time.Since(drillAt).Hours() / 24.0
	}
}

func (r *Registry) Report() RealtimeMetricsReport {
	r.mu.RLock()
	defer r.mu.RUnlock()

	notifications := make(map[string]NotificationMetric, len(r.notificationTotals))
	for provider, totals := range r.notificationTotals {
		notifications[provider] = NotificationMetric{
			Success: totals["success"],
			Failure: totals["failure"],
			P95Ms:   calcP95(r.notificationSamples[provider]),
		}
	}
	return RealtimeMetricsReport{
		ConnectedClients:  r.connectedClients,
		P95SSEMs:          calcP95(r.sseSamples),
		P95WebhookMs:      calcP95(r.webhookSamples),
		TotalWebhooksSent: r.totalWebhooksSent,
		Notifications:     notifications,
	}
}

func (r *Registry) FullSnapshot() TelemetrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now().UTC()

	stateSince := ""
	if !r.streamStateSince.IsZero() {
		stateSince = r.streamStateSince.UTC().Format(time.RFC3339Nano)
	}
	recentFallback := 0
	cutoff := now.Add(-time.Minute)
	for _, recordedAt := range r.fallbackTimes {
		if !recordedAt.Before(cutoff) {
			recentFallback++
		}
	}
	connected := int64(0)
	disconnectedAge := float64(0)
	if r.streamEnabled && r.streamState == "connected" {
		connected = 1
	} else if r.streamEnabled && !r.streamStateSince.IsZero() {
		disconnectedAge = now.Sub(r.streamStateSince).Seconds()
		if disconnectedAge < 0 {
			disconnectedAge = 0
		}
	}
	rtTele := RealtimeTelemetry{
		StreamEnabled:                    r.streamEnabled,
		StreamState:                      r.streamState,
		StreamReason:                     r.streamReason,
		StreamStateSince:                 stateSince,
		StreamConnected:                  connected,
		StreamReconnectTotal:             r.streamReconnects,
		StreamDisconnectTotal:            r.streamDisconnects,
		CoordinatorQueueFullTotal:        r.coordinatorQueueFull,
		FallbackRecoveryTotal:            r.fallbackRecoveries,
		GapRepairTotal:                   r.gapRepairs,
		P95CommitToGatewayMs:             calcP95(r.commitGatewaySamples),
		P95CommitToBrowserSSEMs:          calcP95(r.commitBrowserSamples),
		RecentFallbackRecoveryReconciles: recentFallback,
		StreamDisconnectedAgeSeconds:     disconnectedAge,
		ConnectedClients:                 r.connectedClients,
		P95SSEMs:                         calcP95(r.sseSamples),
		P95WebhookMs:                     calcP95(r.webhookSamples),
		TotalWebhooksSent:                r.totalWebhooksSent,
	}

	// Notifications snapshot
	notifP95 := make(map[string]float64, len(r.notificationSamples))
	totalDelivered := 0
	totalFailed := 0
	byProv := make(map[string]ProviderSnapshot, len(r.notifBacklogByProvider))
	for prov, snap := range r.notifBacklogByProvider {
		p95 := calcP95(r.notificationSamples[prov])
		snap.P95Ms = p95
		if totals, ok := r.notificationTotals[prov]; ok {
			snap.Success = totals["success"]
			snap.Failure = totals["failure"]
			totalDelivered += snap.Success
			totalFailed += snap.Failure
		}
		byProv[prov] = snap
		notifP95[prov] = p95
	}
	notifTele := NotificationTelemetry{
		TotalPending:    r.notifTotalPending,
		TotalDeadLetter: r.notifTotalDeadLetter,
		ByProvider:      byProv,
		RecentP95Ms:     notifP95,
		TotalDelivered:  totalDelivered,
		TotalFailed:     totalFailed,
		IsBacklogStuck:  r.notifBacklogStuck,
	}

	// Mutation gate snapshot
	gateTele := MutationGateTelemetry{
		GateState:        r.gateState,
		Owner:            r.gateOwner,
		Reason:           r.gateReason,
		ExpiresInSeconds: r.gateExpiresIn.Seconds(),
		IsLocked:         r.gateIsLocked,
	}

	// Singleton snapshot
	var hbStr string
	var hbAgeSec float64
	var isStale bool
	if !r.workerHeartbeat.IsZero() {
		hbStr = r.workerHeartbeat.UTC().Format(time.RFC3339)
		hbAgeSec = time.Since(r.workerHeartbeat).Seconds()
		if hbAgeSec < 0 {
			hbAgeSec = 0
		}
		thresh := r.workerStaleThreshold
		if thresh <= 0 {
			thresh = 60 * time.Second
		}
		if hbAgeSec > thresh.Seconds() {
			isStale = true
		}
	}
	singleTele := SingletonTelemetry{
		WorkerRole:          r.workerRole,
		WorkerState:         r.workerState,
		SingletonLockHeld:   r.singletonLock,
		MaintenanceOwner:    "worker",
		DrainState:          r.drainState,
		HeartbeatAt:         hbStr,
		HeartbeatAgeSeconds: hbAgeSec,
		IsStale:             isStale,
	}

	// Deployment snapshot
	var promoAtStr string
	if !r.lastPromotionAt.IsZero() {
		promoAtStr = r.lastPromotionAt.UTC().Format(time.RFC3339)
	}
	deptTele := DeploymentTelemetry{
		ActiveSlot:          r.activeSlot,
		ReleaseCommit:       r.releaseCommit,
		RuntimeRole:         r.runtimeRole,
		SchemaVersion:       r.schemaVersion,
		LastPromotionResult: r.lastPromotionResult,
		LastPromotionAt:     promoAtStr,
		FailoverState:       r.failoverState,
	}

	// Backup & drill snapshot
	var backupAtStr string
	var backupAgeSec float64
	var backupOverdue bool
	if !r.backupAt.IsZero() {
		backupAtStr = r.backupAt.UTC().Format(time.RFC3339)
		backupAgeSec = time.Since(r.backupAt).Seconds()
		backupOverdue = (backupAgeSec > 86400)
	} else if r.backupOverdue {
		backupOverdue = true
	}
	var drillAtStr string
	if !r.restoreDrillAt.IsZero() {
		drillAtStr = r.restoreDrillAt.UTC().Format(time.RFC3339)
	}
	backupTele := BackupAndDrillTelemetry{
		LastBackupArtifact:      r.backupArtifact,
		LastBackupAt:            backupAtStr,
		BackupAgeSeconds:        backupAgeSec,
		IsBackupOverdue:         backupOverdue,
		LastRestoreDrillAt:      drillAtStr,
		LastRestoreDrillSuccess: r.restoreDrillSuccess,
		RestoreDrillAgeDays:     r.restoreDrillAgeDays,
	}

	return TelemetrySnapshot{
		CapturedAt:       now,
		Realtime:         rtTele,
		Notifications:    notifTele,
		MutationGate:     gateTele,
		Singleton:        singleTele,
		Deployment:       deptTele,
		BackupAndRestore: backupTele,
	}
}

func calcP95(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]float64, len(samples))
	copy(cp, samples)
	sort.Float64s(cp)
	idx := int(float64(len(cp)-1) * 0.95)
	return cp[idx]
}
