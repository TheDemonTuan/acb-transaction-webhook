package telemetry

import (
	"sort"
	"sync"
	"time"
)

type RealtimeMetricsReport struct {
	ConnectedClients   int64                         `json:"connectedClients"`
	CircuitBreakerOpen bool                          `json:"circuitBreakerOpen"`
	LastACBPollAt      string                        `json:"lastAcbPollAt,omitempty"`
	P95IngestMs        float64                       `json:"p95IngestMs"`
	P95SSEMs           float64                       `json:"p95SseMs"`
	P95WebhookMs       float64                       `json:"p95WebhookMs"`
	TotalIngested      int                           `json:"totalIngested"`
	TotalWebhooksSent  int                           `json:"totalWebhooksSent"`
	Notifications      map[string]NotificationMetric `json:"notifications"`
}

type NotificationMetric struct {
	Success int     `json:"success"`
	Failure int     `json:"failure"`
	P95Ms   float64 `json:"p95Ms"`
}

type Registry struct {
	mu sync.RWMutex

	// Realtime & HTTP samples
	ingestSamples       []float64
	sseSamples          []float64
	webhookSamples      []float64
	notificationSamples map[string][]float64
	notificationTotals  map[string]map[string]int
	connectedClients    int64
	circuitBreakerOpen  bool
	lastACBPollAt       time.Time
	lastPollDuration    time.Duration
	lastPollStatus      string
	catchUpDay          string
	totalIngested       int
	totalWebhooksSent   int

	// Scheduler telemetry
	schedQueueDepth      map[string]int
	schedTotalDepth      int
	schedCurrentTask     string
	schedCurrentDuration time.Duration
	schedBusy            bool
	schedEnqueued        map[string]int64
	schedStarted         map[string]int64
	schedCompleted       map[string]int64
	schedYielded         map[string]int64
	schedFailed          map[string]int64
	schedOverloaded      int64
	schedSamples         map[string][]float64

	// History jobs telemetry
	historyCounts          map[string]int
	historyTotal           int
	historyOldestQueuedAge time.Duration
	historyStalled         int
	historyPages           int
	historyRows            int

	// Auth lifecycle telemetry
	authHasActive    bool
	authActiveAge    time.Duration
	authActiveStuck  bool
	authSessionState string
	authRecentCount  int
	authCounts       map[string]int

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
		ingestSamples:          make([]float64, 0, 1000),
		sseSamples:             make([]float64, 0, 1000),
		webhookSamples:         make([]float64, 0, 1000),
		notificationSamples:    make(map[string][]float64),
		notificationTotals:     make(map[string]map[string]int),
		schedQueueDepth:        make(map[string]int),
		schedEnqueued:          make(map[string]int64),
		schedStarted:           make(map[string]int64),
		schedCompleted:         make(map[string]int64),
		schedYielded:           make(map[string]int64),
		schedFailed:            make(map[string]int64),
		schedSamples:           make(map[string][]float64),
		historyCounts:          make(map[string]int),
		authCounts:             make(map[string]int),
		notifBacklogByProvider: make(map[string]ProviderSnapshot),
		workerState:            "READY",
		workerStaleThreshold:   60 * time.Second,
		failoverState:          "PRIMARY",
	}
}

func (r *Registry) RecordIngest(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	r.mu.Lock()
	defer r.mu.Unlock()
	r.totalIngested++
	if len(r.ingestSamples) >= 1000 {
		r.ingestSamples = r.ingestSamples[1:]
	}
	r.ingestSamples = append(r.ingestSamples, ms)
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

func (r *Registry) SetCircuitBreaker(open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.circuitBreakerOpen = open
}

func (r *Registry) SetLastACBPollAt(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastACBPollAt = t.UTC()
}

func (r *Registry) SetLastPollDetails(status string, duration time.Duration, catchUpDay string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastPollStatus = status
	r.lastPollDuration = duration
	r.catchUpDay = catchUpDay
}

// Scheduler telemetry methods

func (r *Registry) SetSchedulerQueue(depths map[string]int, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedQueueDepth = make(map[string]int, len(depths))
	for k, v := range depths {
		r.schedQueueDepth[k] = v
	}
	r.schedTotalDepth = total
}

func (r *Registry) SetSchedulerCurrentTask(kind string, d time.Duration, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedCurrentTask = kind
	r.schedCurrentDuration = d
	r.schedBusy = busy
}

func (r *Registry) RecordTaskEnqueued(kind, priority string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedEnqueued[kind]++
}

func (r *Registry) RecordTaskStarted(kind, priority string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedStarted[kind]++
	r.schedBusy = true
	r.schedCurrentTask = kind
}

func (r *Registry) RecordTaskCompleted(kind, priority string, d time.Duration, outcome string, err error) {
	ms := float64(d.Microseconds()) / 1000.0
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil || outcome == "FATAL" {
		r.schedFailed[kind]++
	} else {
		r.schedCompleted[kind]++
	}
	samples := r.schedSamples[kind]
	if len(samples) >= 500 {
		samples = samples[1:]
	}
	r.schedSamples[kind] = append(samples, ms)
	r.schedBusy = false
	r.schedCurrentTask = "IDLE"
}

func (r *Registry) RecordTaskYielded(kind, priority string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedYielded[kind]++
	r.schedBusy = false
}

func (r *Registry) RecordQueueOverloaded(kind, priority string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedOverloaded++
}

// History jobs telemetry

func (r *Registry) SetHistoryJobs(counts map[string]int, oldestQueuedAge time.Duration, stalled int, pages, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.historyCounts = make(map[string]int, len(counts))
	total := 0
	for k, v := range counts {
		r.historyCounts[k] = v
		total += v
	}
	r.historyTotal = total
	r.historyOldestQueuedAge = oldestQueuedAge
	r.historyStalled = stalled
	r.historyPages = pages
	r.historyRows = rows
}

// Auth lifecycle telemetry

func (r *Registry) SetAuthLifecycle(hasActive bool, activeAge time.Duration, stuck bool, sessionState string, recentCount int, counts map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authHasActive = hasActive
	r.authActiveAge = activeAge
	r.authActiveStuck = stuck
	r.authSessionState = sessionState
	r.authRecentCount = recentCount
	r.authCounts = make(map[string]int, len(counts))
	for k, v := range counts {
		r.authCounts[k] = v
	}
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
	var pollAtStr string
	if !r.lastACBPollAt.IsZero() {
		pollAtStr = r.lastACBPollAt.UTC().Format(time.RFC3339)
	}
	return RealtimeMetricsReport{
		ConnectedClients:   r.connectedClients,
		CircuitBreakerOpen: r.circuitBreakerOpen,
		LastACBPollAt:      pollAtStr,
		P95IngestMs:        calcP95(r.ingestSamples),
		P95SSEMs:           calcP95(r.sseSamples),
		P95WebhookMs:       calcP95(r.webhookSamples),
		TotalIngested:      r.totalIngested,
		TotalWebhooksSent:  r.totalWebhooksSent,
		Notifications:      notifications,
	}
}

func (r *Registry) FullSnapshot() TelemetrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now().UTC()

	// 1. Scheduler snapshot
	schedP95 := make(map[string]float64, len(r.schedSamples))
	for k, samples := range r.schedSamples {
		schedP95[k] = calcP95(samples)
	}
	schedTele := SchedulerTelemetry{
		QueueDepthByPriority:  copyIntMap(r.schedQueueDepth),
		TotalQueueDepth:       r.schedTotalDepth,
		CurrentTaskKind:       r.schedCurrentTask,
		CurrentTaskDurationMs: float64(r.schedCurrentDuration.Microseconds()) / 1000.0,
		IsBusy:                r.schedBusy,
		Enqueued:              copyInt64Map(r.schedEnqueued),
		Started:               copyInt64Map(r.schedStarted),
		Completed:             copyInt64Map(r.schedCompleted),
		Yielded:               copyInt64Map(r.schedYielded),
		Failed:                copyInt64Map(r.schedFailed),
		Overloaded:            r.schedOverloaded,
		P95LatencyMs:          schedP95,
	}

	// 2. Realtime snapshot
	var lastPollAtStr string
	var pollAgeSec float64
	if !r.lastACBPollAt.IsZero() {
		lastPollAtStr = r.lastACBPollAt.UTC().Format(time.RFC3339)
		pollAgeSec = time.Since(r.lastACBPollAt).Seconds()
		if pollAgeSec < 0 {
			pollAgeSec = 0
		}
	}
	rtTele := RealtimeTelemetry{
		LastACBPollAt:         lastPollAtStr,
		LastACBPollAgeSeconds: pollAgeSec,
		LastACBPollDurationMs: float64(r.lastPollDuration.Microseconds()) / 1000.0,
		LastACBPollStatus:     r.lastPollStatus,
		CatchUpDay:            r.catchUpDay,
		CircuitBreakerOpen:    r.circuitBreakerOpen,
		ConnectedClients:      r.connectedClients,
		P95IngestMs:           calcP95(r.ingestSamples),
		P95SSEMs:              calcP95(r.sseSamples),
		P95WebhookMs:          calcP95(r.webhookSamples),
		TotalIngested:         r.totalIngested,
		TotalWebhooksSent:     r.totalWebhooksSent,
	}

	// 3. History jobs snapshot
	histTele := HistoryJobsTelemetry{
		CountsByStatus:         copyIntMap(r.historyCounts),
		TotalJobs:              r.historyTotal,
		OldestQueuedAgeSeconds: r.historyOldestQueuedAge.Seconds(),
		StalledCount:           r.historyStalled,
		TotalPagesDone:         r.historyPages,
		TotalRowsSeen:          r.historyRows,
	}

	// 4. Auth snapshot
	authTele := AuthLifecycleTelemetry{
		HasActiveAttempt:        r.authHasActive,
		ActiveAttemptAgeSeconds: r.authActiveAge.Seconds(),
		ActiveAttemptStuck:      r.authActiveStuck,
		SessionState:            r.authSessionState,
		RecentAttemptsCount:     r.authRecentCount,
		AttemptsByStatus:        copyIntMap(r.authCounts),
	}

	// 5. Notifications snapshot
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

	// 6. Mutation gate snapshot
	gateTele := MutationGateTelemetry{
		GateState:        r.gateState,
		Owner:            r.gateOwner,
		Reason:           r.gateReason,
		ExpiresInSeconds: r.gateExpiresIn.Seconds(),
		IsLocked:         r.gateIsLocked,
	}

	// 7. Singleton snapshot
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

	// 8. Deployment snapshot
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

	// 9. Backup & drill snapshot
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
		Scheduler:        schedTele,
		Realtime:         rtTele,
		HistoryJobs:      histTele,
		AuthLifecycle:    authTele,
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

func copyIntMap(m map[string]int) map[string]int {
	cp := make(map[string]int, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func copyInt64Map(m map[string]int64) map[string]int64 {
	cp := make(map[string]int64, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
