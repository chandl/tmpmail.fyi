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
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
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
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	store := &Store{db: db, msgDir: msgDir, cfg: cfg}
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
	defer os.Remove(tmpName)
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
	dbStarted := time.Now()
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
		return nil
	})
	if s.cfg.MetricsEnabled {
		storageSaveStageDuration.WithLabelValues("database", metricResult(err)).Observe(time.Since(dbStarted).Seconds())
	}
	if err != nil {
		s.removeMessageFile(path)
		return nil, err
	}
	s.storedBytes += size
	s.storedMessages += int64(len(recipients))
	stats := cleanupStats{}
	if s.storedBytes > s.cfg.MaxStorageBytes {
		limitStarted := time.Now()
		stats, err = s.enforceLimitLocked()
		if s.cfg.MetricsEnabled {
			storageSaveStageDuration.WithLabelValues("storage_limit", metricResult(err)).Observe(time.Since(limitStarted).Seconds())
		}
		if err != nil {
			return nil, err
		}
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, recipient, sender, subject, received_at, expires_at, size FROM messages WHERE recipient = ? AND expires_at > ? ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?`, recipient, time.Now().Unix(), limit+1, offset)
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	stats, err := s.cleanupLocked(time.Now().Unix())
	if err == nil {
		logCleanup(stats, s.cfg.MetricsEnabled)
		if s.cfg.MetricsEnabled {
			s.updateStorageMetrics()
			s.observeDBStats()
		}
	} else if s.cfg.MetricsEnabled {
		storageErrors.WithLabelValues("cleanup").Inc()
		cleanupErrors.Inc()
	}
	return err
}

func (s *Store) loadStorageUsage() error {
	if err := s.db.QueryRow("SELECT COALESCE(SUM(size), 0) FROM blobs").Scan(&s.storedBytes); err != nil {
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

func (s *Store) enforceLimitLocked() (cleanupStats, error) {
	stats, err := s.cleanupLocked(time.Now().Unix())
	if err != nil {
		return cleanupStats{}, err
	}
	for s.storedBytes > s.cfg.MaxStorageBytes {
		var id, blobID string
		err := s.db.QueryRow("SELECT id, blob_id FROM messages ORDER BY received_at, id LIMIT 1").Scan(&id, &blobID)
		if err == sql.ErrNoRows {
			return stats, nil
		}
		if err != nil {
			return cleanupStats{}, err
		}
		var reclaimed int64
		var removedPath string
		if err := s.inTx(func(tx *sql.Tx) error {
			reclaimed, removedPath, err = deleteMessageTx(tx, id, blobID)
			return err
		}); err != nil {
			return cleanupStats{}, err
		}
		if removedPath != "" {
			s.removeMessageFile(removedPath)
		}
		s.storedBytes -= reclaimed
		s.storedMessages--
		stats.Evicted++
		stats.EvictedBytes += reclaimed
	}
	return stats, nil
}

func (s *Store) cleanupLocked(before int64) (cleanupStats, error) {
	rows, err := s.db.Query("SELECT id, blob_id FROM messages WHERE expires_at <= ?", before)
	if err != nil {
		return cleanupStats{}, err
	}
	var expired []struct {
		id, blobID string
	}
	for rows.Next() {
		var m struct {
			id, blobID string
		}
		if err := rows.Scan(&m.id, &m.blobID); err != nil {
			rows.Close()
			return cleanupStats{}, err
		}
		expired = append(expired, m)
	}
	if err := rows.Close(); err != nil {
		return cleanupStats{}, err
	}
	stats := cleanupStats{}
	var pathsToRemove []string
	for _, m := range expired {
		var reclaimed int64
		var removedPath string
		if err := s.inTx(func(tx *sql.Tx) error {
			reclaimed, removedPath, err = deleteMessageTx(tx, m.id, m.blobID)
			return err
		}); err != nil {
			return cleanupStats{}, err
		}
		stats.Expired++
		stats.ExpiredBytes += reclaimed
		if removedPath != "" {
			pathsToRemove = append(pathsToRemove, removedPath)
		}
	}
	for _, path := range pathsToRemove {
		s.removeMessageFile(path)
	}
	s.storedBytes -= stats.ExpiredBytes
	s.storedMessages -= int64(stats.Expired)
	return stats, nil
}

// deleteMessageTx deletes one message row and releases its reference on the
// shared blob, deleting the blob row once no message references it. It
// returns the blob's path (for the caller to unlink after commit) and its
// size only when the blob was actually removed, since that is the only case
// disk space is reclaimed.
func deleteMessageTx(tx *sql.Tx, id, blobID string) (reclaimedBytes int64, removedPath string, err error) {
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
		if _, err = tx.Exec("DELETE FROM blobs WHERE id = ?", blobID); err != nil {
			return 0, "", err
		}
		return size, path, nil
	}
	if _, err = tx.Exec("UPDATE blobs SET ref_count = ref_count - 1 WHERE id = ?", blobID); err != nil {
		return 0, "", err
	}
	return 0, "", nil
}

func logCleanup(stats cleanupStats, metricsEnabled bool) {
	if stats.Expired > 0 {
		log.Printf("cleanup expired_messages=%d reclaimed_bytes=%d", stats.Expired, stats.ExpiredBytes)
	}
	if stats.Evicted > 0 {
		log.Printf("cleanup storage_evicted=%d reclaimed_bytes=%d", stats.Evicted, stats.EvictedBytes)
	}
	if metricsEnabled {
		observeCleanup(stats)
	}
}

func (s *Store) removeMessageFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("remove message file %s: %v", path, err)
		if s.cfg.MetricsEnabled {
			storageErrors.WithLabelValues("remove_file").Inc()
		}
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
