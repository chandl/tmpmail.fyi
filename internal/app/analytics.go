package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	analyticsQueueSize       = 4096
	analyticsBatchSize       = 128
	analyticsFlushInterval   = 250 * time.Millisecond
	analyticsCleanupInterval = time.Minute
	analyticsRetryInterval   = time.Second
	analyticsMaxAttempts     = 3
)

type AnalyticsStatus struct {
	Enabled            bool       `json:"enabled"`
	Available          bool       `json:"available"`
	StorageBytes       int64      `json:"storageBytes"`
	MaxStorageBytes    int64      `json:"maxStorageBytes"`
	RetentionHours     float64    `json:"retentionHours"`
	EarliestEvent      *time.Time `json:"earliestEvent"`
	LastIngested       *time.Time `json:"lastIngested"`
	LagSeconds         float64    `json:"lagSeconds"`
	DroppedEvents      uint64     `json:"droppedEvents"`
	WriteErrors        uint64     `json:"writeErrors"`
	EarlyEvictedEvents uint64     `json:"earlyEvictedEvents"`
}

// Analytics owns a separate database and a single bounded queue. Its worker is
// the only writer; recording metadata never waits for disk or a database lock.
type queuedAnalyticsEvent struct {
	event    AnalyticsEvent
	queuedAt int64
}

type Analytics struct {
	cfg          Config
	path         string
	queue        chan queuedAnalyticsEvent
	stop         chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	closed       atomic.Bool
	pending      atomic.Int64
	backlogSince atomic.Int64
	mu           sync.RWMutex
	db           *sql.DB
	status       AnalyticsStatus
}

func OpenAnalytics(cfg Config) *Analytics {
	if cfg.AnalyticsEventTTL <= 0 {
		cfg.AnalyticsEventTTL = 720 * time.Hour
	}
	if cfg.AnalyticsMaxStorageBytes <= 0 {
		cfg.AnalyticsMaxStorageBytes = 1073741824
	}
	a := &Analytics{cfg: cfg, path: filepath.Join(cfg.DataDir, "analytics.db"), stop: make(chan struct{}), done: make(chan struct{}), status: AnalyticsStatus{Enabled: cfg.AnalyticsEnabled, RetentionHours: cfg.AnalyticsEventTTL.Hours(), MaxStorageBytes: cfg.AnalyticsMaxStorageBytes}}
	if !cfg.AnalyticsEnabled {
		close(a.done)
		return a
	}
	a.queue = make(chan queuedAnalyticsEvent, analyticsQueueSize)
	a.open()
	go a.run()
	return a
}

func (a *Analytics) DB() *sql.DB {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.db
}
func (a *Analytics) Snapshot() AnalyticsStatus {
	if a == nil {
		return AnalyticsStatus{}
	}
	a.mu.RLock()
	s := a.status
	a.mu.RUnlock()
	if a.pending.Load() > 0 {
		if since := a.backlogSince.Load(); since > 0 {
			s.LagSeconds = time.Since(time.UnixMilli(since)).Seconds()
		}
	}
	return s
}
func (a *Analytics) Record(e AnalyticsEvent) {
	if a == nil || !a.cfg.AnalyticsEnabled || a.closed.Load() {
		return
	}
	if e.Kind != "delivery" && e.Kind != "http" && e.Kind != "smtpRejected" {
		a.drop(1)
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.ID == "" {
		e.ID = newID()
	}
	// Bound every caller-controlled string even when instrumentation is bypassed.
	e.Recipient = boundedAnalyticsString(e.Recipient, 320)
	e.Sender = boundedAnalyticsString(e.Sender, 320)
	e.SenderDomain = boundedAnalyticsString(e.SenderDomain, 255)
	e.IP = boundedAnalyticsString(e.IP, 64)
	e.UserAgent = boundedAnalyticsString(e.UserAgent, 512)
	e.Route = boundedAnalyticsString(e.Route, 128)
	e.Method = boundedAnalyticsString(e.Method, 16)
	e.MessageID = boundedAnalyticsString(e.MessageID, 128)
	e.ID = boundedAnalyticsString(e.ID, 128)
	if a.pending.Add(1) == 1 {
		a.backlogSince.Store(time.Now().UnixMilli())
	}
	select {
	case a.queue <- queuedAnalyticsEvent{event: e, queuedAt: time.Now().UnixMilli()}:
	default:
		a.pending.Add(-1)
		a.drop(1)
	}
}
func boundedAnalyticsString(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	// Copy after truncation so a substring cannot pin an arbitrary-size request
	// allocation in the queue. Drop malformed/truncated UTF-8 bytes.
	return strings.Clone(strings.ToValidUTF8(s, ""))
}
func (a *Analytics) drop(n int) { a.mu.Lock(); a.status.DroppedEvents += uint64(n); a.mu.Unlock() }
func (a *Analytics) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		if a.cfg.AnalyticsEnabled {
			close(a.stop)
		}
	})
	<-a.done
	return nil
}

// Legacy delivery rows keep NULL timing; zero is a valid measured duration.
func migrateDeliveryTiming(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(deliveries)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "duration_ms" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !found {
		_, err = db.Exec(`ALTER TABLE deliveries ADD COLUMN duration_ms REAL`)
	}
	return err
}

func (a *Analytics) open() bool {
	if err := os.MkdirAll(a.cfg.DataDir, 0750); err != nil {
		a.failure()
		return false
	}
	db, err := sql.Open("sqlite3", a.path+"?_journal_mode=WAL&_busy_timeout=100&_auto_vacuum=incremental")
	if err == nil {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS deliveries (id TEXT PRIMARY KEY,timestamp INTEGER NOT NULL,recipient TEXT NOT NULL,sender TEXT NOT NULL,sender_domain TEXT NOT NULL,ip TEXT NOT NULL,size INTEGER NOT NULL,duration_ms REAL)`,
			`CREATE TABLE IF NOT EXISTS smtp_rejections (id TEXT PRIMARY KEY,timestamp INTEGER NOT NULL,recipient TEXT NOT NULL,sender TEXT NOT NULL,sender_domain TEXT NOT NULL,ip TEXT NOT NULL,status INTEGER NOT NULL,duration_ms REAL NOT NULL,stage TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS smtp_rejections_time ON smtp_rejections(timestamp DESC,id)`,
			`CREATE TABLE IF NOT EXISTS http_requests (id TEXT PRIMARY KEY,timestamp INTEGER NOT NULL,recipient TEXT NOT NULL,ip TEXT NOT NULL,user_agent TEXT NOT NULL,route TEXT NOT NULL,method TEXT NOT NULL,status INTEGER NOT NULL,duration_ms REAL NOT NULL,message_id TEXT NOT NULL,polling INTEGER NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS deliveries_time ON deliveries(timestamp DESC,id)`,
			`CREATE INDEX IF NOT EXISTS http_requests_time ON http_requests(timestamp DESC,id)`,
			`CREATE INDEX IF NOT EXISTS deliveries_recipient_time ON deliveries(recipient,timestamp)`,
			`CREATE INDEX IF NOT EXISTS deliveries_domain_time ON deliveries(sender_domain,timestamp)`,
			`CREATE INDEX IF NOT EXISTS http_requests_ip_time ON http_requests(ip,timestamp)`,
		} {
			if _, err = db.Exec(q); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = migrateDeliveryTiming(db)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		a.failure()
		return false
	}
	a.mu.Lock()
	a.db = db
	a.status.Available = true
	a.mu.Unlock()
	if err := a.prune(db); err != nil {
		a.failure()
		a.disconnect()
		return false
	}
	if err := a.maintain(db); err != nil {
		a.failure()
		a.disconnect()
		return false
	}
	return true
}
func (a *Analytics) failure() {
	a.mu.Lock()
	a.status.WriteErrors++
	a.status.Available = false
	a.mu.Unlock()
}
func (a *Analytics) disconnect() {
	a.mu.Lock()
	db := a.db
	a.db = nil
	a.status.Available = false
	a.mu.Unlock()
	if db != nil {
		db.Close()
	}
}

func (a *Analytics) run() {
	defer close(a.done)
	defer a.disconnect()
	flush := time.NewTicker(analyticsFlushInterval)
	defer flush.Stop()
	cleanup := time.NewTicker(analyticsCleanupInterval)
	defer cleanup.Stop()
	retry := time.NewTicker(analyticsRetryInterval)
	defer retry.Stop()
	batch := make([]AnalyticsEvent, 0, analyticsBatchSize)
	attempts := 0
	write := func() {
		if len(batch) == 0 {
			return
		}
		db := a.DB()
		if db == nil {
			return
		}
		if err := a.writeBatch(db, batch); err != nil {
			a.failure()
			attempts++
			if attempts >= analyticsMaxAttempts {
				a.drop(len(batch))
				a.pending.Add(-int64(len(batch)))
				batch = batch[:0]
				attempts = 0
			}
			a.disconnect()
			return
		}
		a.pending.Add(-int64(len(batch)))
		batch = batch[:0]
		attempts = 0
		now := time.Now().UTC()
		a.mu.Lock()
		a.status.LastIngested = &now
		a.status.Available = true
		a.mu.Unlock()
		if err := a.maintain(db); err != nil {
			a.failure()
			a.disconnect()
		}
	}
	for {
		// Stop consuming while an unavailable writer holds a full batch. The queue
		// then overflows predictably instead of growing a second unbounded buffer.
		var input <-chan queuedAnalyticsEvent
		if len(batch) < analyticsBatchSize {
			input = a.queue
		}
		select {
		case <-a.stop:
			// Drain the bounded queue on shutdown while the database is healthy.
			write()
			for len(a.queue) > 0 && a.DB() != nil {
				for len(batch) < analyticsBatchSize && len(a.queue) > 0 {
					queued := <-a.queue
					if len(batch) == 0 {
						a.backlogSince.Store(queued.queuedAt)
					}
					batch = append(batch, queued.event)
				}
				write()
			}
			n := len(batch) + len(a.queue)
			a.drop(n)
			a.pending.Add(-int64(n))
			return
		case queued := <-input:
			if len(batch) == 0 {
				a.backlogSince.Store(queued.queuedAt)
			}
			batch = append(batch, queued.event)
			if len(batch) == analyticsBatchSize {
				write()
			}
		case <-flush.C:
			write()
		case <-retry.C:
			if a.DB() == nil {
				a.open()
			}
			if a.DB() == nil && len(batch) > 0 {
				attempts++
				if attempts >= analyticsMaxAttempts {
					a.drop(len(batch))
					a.pending.Add(-int64(len(batch)))
					batch = batch[:0]
					attempts = 0
				}
			} else {
				write()
			}
		case <-cleanup.C:
			if db := a.DB(); db != nil {
				err := a.prune(db)
				if err == nil {
					err = a.maintain(db)
				}
				if err != nil {
					a.failure()
					a.disconnect()
				}
			}
		}
	}
}
func (a *Analytics) writeBatch(db *sql.DB, events []AnalyticsEvent) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := time.Now().Add(-a.cfg.AnalyticsEventTTL)
	for _, e := range events {
		if e.Timestamp.Before(cutoff) {
			continue
		}
		if e.Kind == "delivery" {
			var timing any
			if e.DurationKnown {
				timing = e.DurationMS
			}
			_, err = tx.Exec(`INSERT OR IGNORE INTO deliveries(id,timestamp,recipient,sender,sender_domain,ip,size,duration_ms) VALUES(?,?,?,?,?,?,?,?)`, e.ID, e.Timestamp.UnixMilli(), e.Recipient, e.Sender, e.SenderDomain, e.IP, e.Size, timing)
		} else if e.Kind == "smtpRejected" {
			_, err = tx.Exec(`INSERT OR IGNORE INTO smtp_rejections(id,timestamp,recipient,sender,sender_domain,ip,status,duration_ms,stage) VALUES(?,?,?,?,?,?,?,?,?)`, e.ID, e.Timestamp.UnixMilli(), e.Recipient, e.Sender, e.SenderDomain, e.IP, e.Status, e.DurationMS, e.Route)
		} else {
			_, err = tx.Exec(`INSERT OR IGNORE INTO http_requests(id,timestamp,recipient,ip,user_agent,route,method,status,duration_ms,message_id,polling) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.Timestamp.UnixMilli(), e.Recipient, e.IP, e.UserAgent, e.Route, e.Method, e.Status, e.DurationMS, e.MessageID, e.Polling)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Effective usage counts live SQLite pages and WAL bytes. Free pages are reusable;
// checkpoint and incremental vacuum reclaim physical space when possible. This
// is a retention budget, not an exact filesystem quota (readers may pin a WAL).
func (a *Analytics) usage(db *sql.DB) (int64, error) {
	var pages, free, size int64
	for _, q := range []struct {
		sql string
		dst *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &size}} {
		if err := db.QueryRow(q.sql).Scan(q.dst); err != nil {
			return 0, err
		}
	}
	used := (pages - free) * size
	if info, err := os.Stat(a.path + "-wal"); err == nil {
		used += info.Size()
	}
	return used, nil
}
func (a *Analytics) prune(db *sql.DB) error {
	cutoff := time.Now().Add(-a.cfg.AnalyticsEventTTL).UnixMilli()
	for _, table := range []string{"deliveries", "http_requests", "smtp_rejections"} {
		if _, err := db.Exec("DELETE FROM "+table+" WHERE timestamp < ?", cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (a *Analytics) maintain(db *sql.DB) error {
	used, err := a.usage(db)
	if err != nil {
		return err
	}
	if used > a.cfg.AnalyticsMaxStorageBytes {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return err
		}
		used, err = a.usage(db)
		if err != nil {
			return err
		}
		for rounds := 0; used > a.cfg.AnalyticsMaxStorageBytes && rounds < 32; rounds++ {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			rows, err := tx.Query(`SELECT kind,id FROM (SELECT 'deliveries' kind,id,timestamp FROM deliveries UNION ALL SELECT 'http_requests',id,timestamp FROM http_requests UNION ALL SELECT 'smtp_rejections',id,timestamp FROM smtp_rejections) ORDER BY timestamp,id LIMIT 256`)
			if err != nil {
				tx.Rollback()
				return err
			}
			type key struct{ table, id string }
			keys := []key{}
			for rows.Next() {
				var k key
				if err := rows.Scan(&k.table, &k.id); err != nil {
					rows.Close()
					tx.Rollback()
					return err
				}
				keys = append(keys, k)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				tx.Rollback()
				return err
			}
			for _, k := range keys {
				if _, err = tx.Exec("DELETE FROM "+k.table+" WHERE id=?", k.id); err != nil {
					tx.Rollback()
					return err
				}
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			a.mu.Lock()
			a.status.EarlyEvictedEvents += uint64(len(keys))
			a.mu.Unlock()
			if len(keys) == 0 {
				break
			}
			if _, err = db.Exec("PRAGMA incremental_vacuum"); err != nil {
				return err
			}
			if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				return err
			}
			used, err = a.usage(db)
			if err != nil {
				return err
			}
		}
	}
	var earliest sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(timestamp) FROM (SELECT MIN(timestamp) timestamp FROM deliveries UNION ALL SELECT MIN(timestamp) FROM http_requests UNION ALL SELECT MIN(timestamp) FROM smtp_rejections)`).Scan(&earliest); err != nil {
		return fmt.Errorf("analytics history: %w", err)
	}
	var first *time.Time
	if earliest.Valid {
		t := time.UnixMilli(earliest.Int64).UTC()
		first = &t
	}
	a.mu.Lock()
	a.status.StorageBytes = used
	a.status.EarliestEvent = first
	a.mu.Unlock()
	return nil
}
