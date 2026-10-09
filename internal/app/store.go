package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Message struct {
	ID        string    `json:"id"`
	Recipient string    `json:"recipient"`
	From      string    `json:"from"`
	Subject   string    `json:"subject"`
	Received  time.Time `json:"received"`
	ExpiresAt time.Time `json:"expiresAt"`
	Size      int64     `json:"size"`
	Body      string    `json:"body,omitempty"`
}

type Store struct {
	db             *sql.DB
	msgDir         string
	cfg            Config
	writeMu        sync.Mutex
	storedBytes    int64
	storedMessages int64
	// removeFile is replaceable by tests to exercise unlink failures.
	removeFile    func(string) error
	storageFault  error
	pendingCursor string
}

type cleanupStats struct {
	Expired      int
	ExpiredBytes int64
	Evicted      int
	EvictedBytes int64
}

func OpenStore(dbPath, msgDir string, cfg Config) (*Store, error) {
	if err := os.MkdirAll(msgDir, 0o750); err != nil {
		return nil, err
	}
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// WAL permits concurrent readers. Writes remain serialized by writeMu so
	// SQLite still has one writer while message reads do not queue behind it.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := migrateLegacySchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS blobs (
			id TEXT PRIMARY KEY, path TEXT NOT NULL, size INTEGER NOT NULL, ref_count INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY, recipient TEXT NOT NULL, sender TEXT NOT NULL,
			subject TEXT NOT NULL, received_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			size INTEGER NOT NULL, blob_id TEXT NOT NULL)`,
		"CREATE INDEX IF NOT EXISTS messages_recipient_received ON messages(recipient, received_at DESC)",
		"CREATE INDEX IF NOT EXISTS messages_expiry ON messages(expires_at)",
		"CREATE INDEX IF NOT EXISTS messages_blob ON messages(blob_id)",
		"CREATE INDEX IF NOT EXISTS messages_received ON messages(received_at)",
		`CREATE TABLE IF NOT EXISTS pending_deletions (path TEXT PRIMARY KEY, size INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	store := &Store{db: db, msgDir: msgDir, cfg: cfg, removeFile: os.Remove}
	if err := store.reconcileFiles(); err != nil {
		db.Close()
		return nil, fmt.Errorf("reconcile message files: %w", err)
	}
	if err := store.loadStorageUsage(); err != nil {
		db.Close()
		return nil, err
	}
	if cfg.MetricsEnabled {
		store.updateStorageMetrics()
		store.observeDBStats()
	}
	return store, nil
}

// migrateLegacySchema upgrades a pre-dedup database (one file per message,
// messages.path) to the blob-backed schema (messages.blob_id referencing a
// shared, ref-counted blobs row). It is a no-op on a fresh database or one
// that has already been migrated.
func migrateLegacySchema(db *sql.DB) error {
	hasMessages, err := tableExists(db, "messages")
	if err != nil || !hasMessages {
		return err
	}
	hasPath, err := columnExists(db, "messages", "path")
	if err != nil || !hasPath {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS blobs (
		id TEXT PRIMARY KEY, path TEXT NOT NULL, size INTEGER NOT NULL, ref_count INTEGER NOT NULL)`); err != nil {
		return err
	}
	hasBlobID, err := columnExists(db, "messages", "blob_id")
	if err != nil {
		return err
	}
	if !hasBlobID {
		if _, err := tx.Exec(`ALTER TABLE messages ADD COLUMN blob_id TEXT`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO blobs (id, path, size, ref_count) SELECT id, path, size, 1 FROM messages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE messages SET blob_id = id`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE messages DROP COLUMN path`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count > 0, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Save writes raw once and creates one Message per recipient, all sharing a
// single ref-counted blob so a multi-recipient delivery is not stored once
// per recipient.
func (s *Store) Save(recipients []string, sender string, raw []byte) (messages []Message, err error) {
	started := time.Now()
	defer func() {
		if s.cfg.MetricsEnabled {
			storageSaveDuration.WithLabelValues(metricResult(err)).Observe(time.Since(started).Seconds())
			if err != nil {
				storageErrors.WithLabelValues("save").Inc()
			}
		}
	}()
	seen := make(map[string]bool, len(recipients))
	normalized := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		recipient = normalizeRecipient(recipient)
		if !seen[recipient] {
			normalized = append(normalized, recipient)
			seen[recipient] = true
		}
	}
	recipients = normalized
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients")
	}
	if int64(len(raw)) > s.cfg.MaxMessageBytes {
		return nil, fmt.Errorf("message exceeds %d byte limit", s.cfg.MaxMessageBytes)
	}
	if int64(len(raw)) > s.cfg.MaxStorageBytes {
		return nil, fmt.Errorf("message exceeds %d byte storage limit", s.cfg.MaxStorageBytes)
	}
	lockStarted := time.Now()
	s.writeMu.Lock()
	if s.cfg.MetricsEnabled {
		storageWriteLockWait.Observe(time.Since(lockStarted).Seconds())
	}
	defer s.writeMu.Unlock()
	if s.storageFault != nil {
		return nil, fmt.Errorf("storage recovery required: %w", s.storageFault)
	}
	// Retry durable unlink work before admitting more disk usage.
	if err := s.drainPendingLocked(); err != nil {
		log.Printf("retry pending message deletion: %v", err)
	}
	if s.storedBytes > s.cfg.MaxStorageBytes {
		return nil, fmt.Errorf("storage remains above limit awaiting file deletion")
	}
	var pendingBytes int64
	if err := s.db.QueryRow("SELECT COALESCE(SUM(size), 0) FROM pending_deletions").Scan(&pendingBytes); err != nil {
		return nil, err
	}
	if pendingBytes+int64(len(raw)) > s.cfg.MaxStorageBytes {
		return nil, fmt.Errorf("storage limit exhausted by pending message files")
	}
	now := time.Now().UTC()
	subject, from := mailDetails(raw, sender)
	size := int64(len(raw))
	blobID := newID()
	path := filepath.Join(s.msgDir, blobID+".eml")
	tmp, err := os.CreateTemp(s.msgDir, ".incoming-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer func() {
		if err := s.removeFile(tmpName); err != nil && !os.IsNotExist(err) {
			info, statErr := os.Stat(tmpName)
			if statErr == nil {
				s.discardFileLocked(tmpName, info.Size())
			}
		}
	}()
	fileStarted := time.Now()
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if s.cfg.MetricsEnabled {
		storageSaveStageDuration.WithLabelValues("file", metricResult(err)).Observe(time.Since(fileStarted).Seconds())
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return nil, err
	}
	if err = syncDirectory(s.msgDir); err != nil {
		s.discardFileLocked(path, size)
		return nil, err
	}
	dbStarted := time.Now()
	stats := cleanupStats{}
	messages = make([]Message, len(recipients))
	err = s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO blobs (id, path, size, ref_count) VALUES (?, ?, ?, ?)`, blobID, path, size, len(recipients)); err != nil {
			return err
		}
		for i, recipient := range recipients {
			m := Message{ID: newID(), Recipient: recipient, From: from, Subject: subject, Received: now, ExpiresAt: now.Add(s.cfg.MessageTTL), Size: size}
			if _, err := tx.Exec(`INSERT INTO messages (id, recipient, sender, subject, received_at, expires_at, size, blob_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.Recipient, from, m.Subject, m.Received.Unix(), m.ExpiresAt.Unix(), m.Size, blobID); err != nil {
				return err
			}
			messages[i] = m
		}
		// Eviction and insertion must commit together: SMTP retries must never
		// follow a failed Save that nevertheless inserted a readable message.
		var limitErr error
		stats, limitErr = s.evictTx(tx, blobID, size)
		return limitErr
	})
	if s.cfg.MetricsEnabled {
		storageSaveStageDuration.WithLabelValues("database", metricResult(err)).Observe(time.Since(dbStarted).Seconds())
	}
	if err != nil {
		s.discardFileLocked(path, size)
		return nil, err
	}
	// The delivery is durable now. An unlink failure must not turn successful
	// acceptance into an SMTP retry; pending bytes stay charged to the cap.
	s.storedBytes += size
	s.storedMessages += int64(len(recipients) - stats.Evicted - stats.Expired)
	if err := s.drainPendingLocked(); err != nil {
		log.Printf("remove evicted message files: %v", err)
	}
	logCleanup(stats, s.cfg.MetricsEnabled)
	if s.cfg.MetricsEnabled {
		s.updateStorageMetrics()
		s.observeDBStats()
	}
	return messages, nil
}

func (s *Store) List(recipient string) ([]Message, error) {
	messages, _, err := s.ListPage(recipient, 100, 0)
	return messages, err
}

func (s *Store) ListPage(recipient string, limit, offset int) (messages []Message, hasMore bool, err error) {
	return s.ListPageContext(context.Background(), recipient, limit, offset)
}

func (s *Store) ListPageContext(ctx context.Context, recipient string, limit, offset int) (messages []Message, hasMore bool, err error) {
	started := time.Now()
	defer func() {
		if s.cfg.MetricsEnabled {
			storageReadDuration.WithLabelValues("list", metricResult(err)).Observe(time.Since(started).Seconds())
			s.observeDBStats()
		}
	}()
	recipient = normalizeRecipient(recipient)
	rows, err := s.db.QueryContext(ctx, `SELECT id, recipient, sender, subject, received_at, expires_at, size FROM messages WHERE recipient = ? AND expires_at > ? ORDER BY received_at DESC, rowid DESC LIMIT ? OFFSET ?`, recipient, time.Now().Unix(), limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var received, expires int64
		if err := rows.Scan(&m.ID, &m.Recipient, &m.From, &m.Subject, &received, &expires, &m.Size); err != nil {
			return nil, false, err
		}
		m.Received, m.ExpiresAt = time.Unix(received, 0).UTC(), time.Unix(expires, 0).UTC()
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore = len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	return messages, hasMore, nil
}

func (s *Store) Get(id string) (m Message, err error) {
	return s.GetContext(context.Background(), id)
}

func (s *Store) GetContext(ctx context.Context, id string) (m Message, err error) {
	started := time.Now()
	defer func() {
		if s.cfg.MetricsEnabled {
			storageReadDuration.WithLabelValues("get", metricResult(err)).Observe(time.Since(started).Seconds())
			s.observeDBStats()
		}
	}()
	var received, expires int64
	var path string
	err = s.db.QueryRowContext(ctx, `SELECT messages.id, messages.recipient, messages.sender, messages.subject, messages.received_at, messages.expires_at, messages.size, blobs.path
		FROM messages JOIN blobs ON blobs.id = messages.blob_id
		WHERE messages.id = ? AND messages.expires_at > ?`, id, time.Now().Unix()).Scan(&m.ID, &m.Recipient, &m.From, &m.Subject, &received, &expires, &m.Size, &path)
	if err != nil {
		return Message{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Message{}, err
	}
	m.Received, m.ExpiresAt, m.Body = time.Unix(received, 0).UTC(), time.Unix(expires, 0).UTC(), string(body)
	return m, nil
}

func (s *Store) RunCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Cleanup()
		}
	}
}

func (s *Store) Cleanup() (err error) {
	started := time.Now()
	defer func() {
		if s.cfg.MetricsEnabled {
			cleanupDuration.WithLabelValues(metricResult(err)).Observe(time.Since(started).Seconds())
		}
	}()
	before := time.Now().Unix()
	for {
		s.writeMu.Lock()
		batch, batchErr := s.cleanupBatchLocked(before)
		logCleanup(batch, s.cfg.MetricsEnabled)
		if s.cfg.MetricsEnabled {
			s.updateStorageMetrics()
			s.observeDBStats()
		}
		s.writeMu.Unlock()
		if batchErr != nil {
			if s.cfg.MetricsEnabled {
				storageErrors.WithLabelValues("cleanup").Inc()
				cleanupErrors.Inc()
			}
			return batchErr
		}
		if batch.Expired < cleanupBatchSize {
			return nil
		}
		// Release the writer between bounded transactions so SMTP can progress.
	}
}

func (s *Store) loadStorageUsage() error {
	if err := s.db.QueryRow("SELECT (SELECT COALESCE(SUM(size), 0) FROM blobs) + (SELECT COALESCE(SUM(size), 0) FROM pending_deletions)").Scan(&s.storedBytes); err != nil {
		return err
	}
	return s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&s.storedMessages)
}

func (s *Store) updateStorageMetrics() {
	observeStorageUsage(s.storedBytes, s.storedMessages)
}

func (s *Store) observeDBStats() {
	stats := s.db.Stats()
	storageDBOpenConnections.Set(float64(stats.OpenConnections))
	storageDBInUseConnections.Set(float64(stats.InUse))
}

const cleanupBatchSize = 256

// evictTx enforces the projected post-unlink usage. Existing pending files
// cannot be credited again: if they cannot be removed they restrict admission.
func (s *Store) evictTx(tx *sql.Tx, protectedBlobID string, addedBytes int64) (cleanupStats, error) {
	stats := cleanupStats{}
	projected := s.storedBytes + addedBytes
	before := time.Now().Unix()
	expiredRemain := true
	for projected > s.cfg.MaxStorageBytes {
		var id, blobID string
		// The received_at index contains rowid as its implicit tie breaker.
		// Protect the incoming delivery even when the wall clock moved back.
		expired := false
		var err error
		if expiredRemain {
			err = tx.QueryRow(`SELECT id, blob_id FROM messages WHERE expires_at <= ? AND blob_id != ?
				ORDER BY expires_at, rowid LIMIT 1`, before, protectedBlobID).Scan(&id, &blobID)
			if err == nil {
				expired = true
			} else if err == sql.ErrNoRows {
				expiredRemain = false
			} else {
				return stats, err
			}
		}
		if !expired {
			err = tx.QueryRow(`SELECT id, blob_id FROM messages WHERE blob_id != ?
				ORDER BY received_at, rowid LIMIT 1`, protectedBlobID).Scan(&id, &blobID)
			if err == sql.ErrNoRows {
				return stats, fmt.Errorf("storage limit exhausted by pending files or incoming delivery")
			}
			if err != nil {
				return stats, err
			}
		}
		released, _, err := deleteMessageTx(tx, id, blobID)
		if err != nil {
			return stats, err
		}
		projected -= released
		if expired {
			stats.Expired++
			stats.ExpiredBytes += released
		} else {
			stats.Evicted++
			stats.EvictedBytes += released
		}
	}
	return stats, nil
}

func (s *Store) enforceLimitLocked(protectedBlobID string) (cleanupStats, error) {
	stats, err := s.cleanupLocked(time.Now().Unix())
	if err != nil {
		return stats, err
	}
	var evicted cleanupStats
	if err := s.inTx(func(tx *sql.Tx) error {
		var err error
		evicted, err = s.evictTx(tx, protectedBlobID, 0)
		return err
	}); err != nil {
		return stats, err
	}
	stats.Expired += evicted.Expired
	stats.ExpiredBytes += evicted.ExpiredBytes
	stats.Evicted += evicted.Evicted
	stats.EvictedBytes += evicted.EvictedBytes
	s.storedMessages -= int64(evicted.Evicted + evicted.Expired)
	return stats, s.drainPendingLocked()
}

func (s *Store) cleanupLocked(before int64) (cleanupStats, error) {
	stats := cleanupStats{}
	for {
		batch, err := s.cleanupBatchLocked(before)
		stats.Expired += batch.Expired
		stats.ExpiredBytes += batch.ExpiredBytes
		if err != nil {
			return stats, err
		}
		if batch.Expired < cleanupBatchSize {
			return stats, nil
		}
	}
}

func (s *Store) cleanupBatchLocked(before int64) (cleanupStats, error) {
	batch := cleanupStats{}
	err := s.inTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id, blob_id FROM messages WHERE expires_at <= ? LIMIT ?`, before, cleanupBatchSize)
		if err != nil {
			return err
		}
		type entry struct{ id, blobID string }
		var expired []entry
		for rows.Next() {
			var m entry
			if err := rows.Scan(&m.id, &m.blobID); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, m := range expired {
			released, _, err := deleteMessageTx(tx, m.id, m.blobID)
			if err != nil {
				return err
			}
			batch.Expired++
			batch.ExpiredBytes += released
		}
		return nil
	})
	if err != nil {
		return cleanupStats{}, err
	}
	s.storedMessages -= int64(batch.Expired)
	return batch, s.drainPendingLocked()
}

// Last-reference removal transfers bytes to a durable unlink queue inside the
// same transaction. Bytes are reclaimed only after unlink and directory sync.
func deleteMessageTx(tx *sql.Tx, id, blobID string) (releasedBytes int64, removedPath string, err error) {
	if _, err = tx.Exec("DELETE FROM messages WHERE id = ?", id); err != nil {
		return 0, "", err
	}
	var refCount int
	var path string
	var size int64
	if err = tx.QueryRow("SELECT ref_count, path, size FROM blobs WHERE id = ?", blobID).Scan(&refCount, &path, &size); err != nil {
		return 0, "", err
	}
	if refCount <= 1 {
		if _, err = tx.Exec("INSERT INTO pending_deletions(path, size) VALUES (?, ?)", path, size); err != nil {
			return 0, "", err
		}
		if _, err = tx.Exec("DELETE FROM blobs WHERE id = ?", blobID); err != nil {
			return 0, "", err
		}
		return size, path, nil
	}
	_, err = tx.Exec("UPDATE blobs SET ref_count = ref_count - 1 WHERE id = ?", blobID)
	return 0, "", err
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// drainPendingLocked leaves failed entries charged and retries on Save,
// Cleanup, and restart. Removing a queue entry before directory sync could
// resurrect an unaccounted file following a power failure.
func (s *Store) drainPendingLocked() error {
	// Bound normal writer-lock work and advance past failed entries. Reset
	// after reaching the end so every failure eventually receives another try.
	next, count, err := s.drainPendingBatchLocked(s.pendingCursor)
	if next != "" {
		s.pendingCursor = next
	}
	if count < cleanupBatchSize {
		s.pendingCursor = ""
	}
	return err
}

// Startup can visit the entire queue before requests are admitted.
func (s *Store) drainAllPendingLocked() error {
	var cursor string
	var firstErr error
	for {
		next, count, err := s.drainPendingBatchLocked(cursor)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// SQL/sync failures do not return a cursor. Unlink failures do:
			// continue past them so one stubborn file cannot starve retries.
			if next == "" {
				return firstErr
			}
		}
		if count < cleanupBatchSize {
			return firstErr
		}
		cursor = next
	}
}

func (s *Store) drainPendingBatchLocked(cursor string) (next string, count int, result error) {
	rows, err := s.db.Query("SELECT path, size FROM pending_deletions WHERE path > ? ORDER BY path LIMIT ?", cursor, cleanupBatchSize)
	if err != nil {
		return "", 0, err
	}
	type pendingFile struct {
		path string
		size int64
	}
	var paths []pendingFile
	for rows.Next() {
		var file pendingFile
		if err := rows.Scan(&file.path, &file.size); err != nil {
			rows.Close()
			return "", 0, err
		}
		paths = append(paths, file)
		next = file.path
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", 0, err
	}
	var firstErr error
	var removed []string
	var reclaimed int64
	for _, file := range paths {
		path := file.path
		if err := s.removeFile(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			if s.cfg.MetricsEnabled {
				storageErrors.WithLabelValues("remove_file").Inc()
			}
			continue
		}
		removed = append(removed, path)
		reclaimed += file.size
	}
	if len(removed) > 0 {
		if err := syncDirectory(s.msgDir); err != nil {
			return "", 0, err
		}
		if err := s.inTx(func(tx *sql.Tx) error {
			for _, path := range removed {
				if _, err := tx.Exec("DELETE FROM pending_deletions WHERE path = ?", path); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return "", 0, err
		}
	}
	s.storedBytes -= reclaimed
	if s.cfg.MetricsEnabled {
		s.updateStorageMetrics()
	}
	return next, len(paths), firstErr
}

func (s *Store) discardFileLocked(path string, size int64) {
	// A rolled-back Save can still leave a file. Persist its retry/accounting
	// record; if the DB itself is unavailable, startup scanning recovers it.
	if _, err := s.db.Exec("INSERT OR IGNORE INTO pending_deletions(path, size) VALUES (?, ?)", path, size); err != nil {
		log.Printf("record discarded message file: %v", err)
		if err := s.removeFile(path); err != nil && !os.IsNotExist(err) {
			log.Printf("discard message file: %v", err)
			s.storedBytes += size
			s.storageFault = err
		}
		_ = syncDirectory(s.msgDir)
		return
	}
	if err := s.loadStorageUsage(); err != nil {
		log.Printf("reload discarded storage usage: %v", err)
		return
	}
	if err := s.drainPendingLocked(); err != nil {
		log.Printf("discard message file: %v", err)
	}
}

// Reconciliation runs before serving requests. A missing referenced file is
// surfaced as an error rather than silently deleting accepted mail. Files
// created before a crashed DB commit become durable deletion work.
func (s *Store) reconcileFiles() error {
	if err := s.normalizeStoredRecipients(); err != nil {
		return err
	}
	rows, err := s.db.Query("SELECT path FROM blobs")
	if err != nil {
		return err
	}
	referenced := make(map[string]bool)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		referenced[filepath.Clean(path)] = true
		if _, err := os.Stat(path); err != nil {
			rows.Close()
			return fmt.Errorf("referenced blob %s: %w", path, err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.msgDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasPrefix(entry.Name(), ".incoming-") && !strings.HasSuffix(entry.Name(), ".eml")) {
			continue
		}
		path := filepath.Join(s.msgDir, entry.Name())
		if referenced[filepath.Clean(path)] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if _, err := s.db.Exec("INSERT OR IGNORE INTO pending_deletions(path, size) VALUES (?, ?)", path, info.Size()); err != nil {
			return err
		}
	}
	if err := s.loadStorageUsage(); err != nil {
		return err
	}
	if err := s.drainAllPendingLocked(); err != nil {
		log.Printf("startup pending message deletion: %v", err)
	}
	return nil
}

func (s *Store) normalizeStoredRecipients() error {
	rows, err := s.db.Query("SELECT DISTINCT recipient FROM messages")
	if err != nil {
		return err
	}
	var recipients []string
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			rows.Close()
			return err
		}
		if recipient != normalizeRecipient(recipient) {
			recipients = append(recipients, recipient)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return nil
	}
	return s.inTx(func(tx *sql.Tx) error {
		for _, recipient := range recipients {
			if _, err := tx.Exec("UPDATE messages SET recipient = ? WHERE recipient = ?", normalizeRecipient(recipient), recipient); err != nil {
				return err
			}
		}
		return nil
	})
}

func logCleanup(stats cleanupStats, metricsEnabled bool) {
	if stats.Expired > 0 {
		log.Printf("cleanup expired_messages=%d released_blob_bytes=%d", stats.Expired, stats.ExpiredBytes)
	}
	if stats.Evicted > 0 {
		log.Printf("cleanup storage_evicted=%d released_blob_bytes=%d", stats.Evicted, stats.EvictedBytes)
	}
	if metricsEnabled {
		observeCleanup(stats)
	}
}

func newID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func mailDetails(raw []byte, fallback string) (string, string) {
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return "(no subject)", fallback
	}
	decoder := new(mime.WordDecoder)
	subject, err := decoder.DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject == "" {
		subject = "(no subject)"
	}
	from, err := decoder.DecodeHeader(m.Header.Get("From"))
	if err != nil || from == "" {
		from = fallback
	}
	return subject, from
}
