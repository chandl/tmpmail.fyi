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
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY, recipient TEXT NOT NULL, sender TEXT NOT NULL,
			subject TEXT NOT NULL, received_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			size INTEGER NOT NULL, path TEXT NOT NULL)`,
		"CREATE INDEX IF NOT EXISTS messages_recipient_received ON messages(recipient, received_at DESC)",
		"CREATE INDEX IF NOT EXISTS messages_expiry ON messages(expires_at)",
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

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Save(recipient, sender string, raw []byte) (message Message, err error) {
	started := time.Now()
	defer func() {
		if s.cfg.MetricsEnabled {
			storageSaveDuration.WithLabelValues(metricResult(err)).Observe(time.Since(started).Seconds())
			if err != nil {
				storageErrors.WithLabelValues("save").Inc()
			}
		}
	}()
	if int64(len(raw)) > s.cfg.MaxMessageBytes {
		return Message{}, fmt.Errorf("message exceeds %d byte limit", s.cfg.MaxMessageBytes)
	}
	if int64(len(raw)) > s.cfg.MaxStorageBytes {
		return Message{}, fmt.Errorf("message exceeds %d byte storage limit", s.cfg.MaxStorageBytes)
	}
	lockStarted := time.Now()
	s.writeMu.Lock()
	if s.cfg.MetricsEnabled {
		storageWriteLockWait.Observe(time.Since(lockStarted).Seconds())
	}
	defer s.writeMu.Unlock()
	now := time.Now().UTC()
	message = Message{ID: newID(), Recipient: recipient, From: sender, Received: now, ExpiresAt: now.Add(s.cfg.MessageTTL), Size: int64(len(raw))}
	message.Subject, message.From = mailDetails(raw, sender)
	path := filepath.Join(s.msgDir, message.ID+".eml")
	tmp, err := os.CreateTemp(s.msgDir, ".incoming-*")
	if err != nil {
		return Message{}, err
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
		return Message{}, err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return Message{}, err
	}
	dbStarted := time.Now()
	if _, err = s.db.Exec(`INSERT INTO messages (id, recipient, sender, subject, received_at, expires_at, size, path) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, message.ID, message.Recipient, message.From, message.Subject, message.Received.Unix(), message.ExpiresAt.Unix(), message.Size, path); err != nil {
		s.removeMessageFile(path)
		if s.cfg.MetricsEnabled {
			storageSaveStageDuration.WithLabelValues("database", metricResult(err)).Observe(time.Since(dbStarted).Seconds())
		}
		return Message{}, err
	}
	if s.cfg.MetricsEnabled {
		storageSaveStageDuration.WithLabelValues("database", "success").Observe(time.Since(dbStarted).Seconds())
	}
	s.storedBytes += message.Size
	s.storedMessages++
	stats := cleanupStats{}
	if s.storedBytes > s.cfg.MaxStorageBytes {
		limitStarted := time.Now()
		stats, err = s.enforceLimitLocked()
		if s.cfg.MetricsEnabled {
			storageSaveStageDuration.WithLabelValues("storage_limit", metricResult(err)).Observe(time.Since(limitStarted).Seconds())
		}
		if err != nil {
			return Message{}, err
		}
	}
	logCleanup(stats, s.cfg.MetricsEnabled)
	if s.cfg.MetricsEnabled {
		s.updateStorageMetrics()
		s.observeDBStats()
	}
	return message, nil
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
	err = s.db.QueryRowContext(ctx, `SELECT id, recipient, sender, subject, received_at, expires_at, size, path FROM messages WHERE id = ? AND expires_at > ?`, id, time.Now().Unix()).Scan(&m.ID, &m.Recipient, &m.From, &m.Subject, &received, &expires, &m.Size, &path)
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
	return s.db.QueryRow("SELECT COALESCE(SUM(size), 0), COUNT(*) FROM messages").Scan(&s.storedBytes, &s.storedMessages)
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
		var id, path string
		var size int64
		err := s.db.QueryRow("SELECT id, path, size FROM messages ORDER BY received_at LIMIT 1").Scan(&id, &path, &size)
		if err == sql.ErrNoRows {
			return stats, nil
		}
		if err != nil {
			return cleanupStats{}, err
		}
		if _, err := s.db.Exec("DELETE FROM messages WHERE id = ?", id); err != nil {
			return cleanupStats{}, err
		}
		s.removeMessageFile(path)
		s.storedBytes -= size
		s.storedMessages--
		stats.Evicted++
		stats.EvictedBytes += size
	}
	return stats, nil
}

func (s *Store) cleanupLocked(before int64) (cleanupStats, error) {
	rows, err := s.db.Query("SELECT id, path, size FROM messages WHERE expires_at <= ?", before)
	if err != nil {
		return cleanupStats{}, err
	}
	var expired []struct {
		id, path string
		size     int64
	}
	for rows.Next() {
		var m struct {
			id, path string
			size     int64
		}
		if err := rows.Scan(&m.id, &m.path, &m.size); err != nil {
			rows.Close()
			return cleanupStats{}, err
		}
		expired = append(expired, m)
	}
	if err := rows.Close(); err != nil {
		return cleanupStats{}, err
	}
	stats := cleanupStats{}
	for _, m := range expired {
		if _, err := s.db.Exec("DELETE FROM messages WHERE id = ?", m.id); err != nil {
			return cleanupStats{}, err
		}
		s.removeMessageFile(m.path)
		s.storedBytes -= m.size
		s.storedMessages--
		stats.Expired++
		stats.ExpiredBytes += m.size
	}
	return stats, nil
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
