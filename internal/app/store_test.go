package app

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, ttl time.Duration) *Store {
	t.Helper()
	cfg := Config{MailDomain: "mail.test", MessageTTL: ttl, MaxMessageBytes: 1024 * 1024, MaxStorageBytes: 1024 * 1024}
	store, err := OpenStore(filepath.Join(t.TempDir(), "mail.db"), filepath.Join(t.TempDir(), "messages"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func saveOne(t *testing.T, store *Store, recipient, sender string, raw []byte) Message {
	t.Helper()
	messages, err := store.Save([]string{recipient}, sender, raw)
	if err != nil {
		t.Fatal(err)
	}
	return messages[0]
}

func TestStorageOperationsEmitLoadTestMetrics(t *testing.T) {
	cfg := Config{MailDomain: "mail.test", MessageTTL: time.Hour, MaxMessageBytes: 1024 * 1024, MaxStorageBytes: 1024 * 1024, MetricsEnabled: true}
	dataDir := t.TempDir()
	store, err := OpenStore(filepath.Join(dataDir, "mail.db"), filepath.Join(dataDir, "messages"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Subject: metrics\r\n\r\nbody"))
	if _, _, err := store.ListPage(message.Recipient, 25, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(message.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewMetricsServer().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, metric := range []string{
		"tmpmail_storage_save_duration_seconds",
		"tmpmail_storage_save_stage_duration_seconds",
		"tmpmail_storage_write_lock_wait_seconds",
		"tmpmail_storage_read_duration_seconds",
		"tmpmail_storage_db_open_connections",
		"tmpmail_cleanup_duration_seconds",
	} {
		if !strings.Contains(response.Body.String(), metric) {
			t.Fatalf("expected %s in metrics output", metric)
		}
	}
}

func TestStoreSavesAndListsMessage(t *testing.T) {
	store := testStore(t, time.Hour)
	message := saveOne(t, store, "build@mail.test", "sender@example.org", []byte("From: Sender <sender@example.org>\r\nSubject: Hello ✓\r\n\r\nThe body"))
	list, err := store.List("build@mail.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != message.ID || list[0].Subject != "Hello ✓" {
		t.Fatalf("unexpected list: %#v", list)
	}
	got, err := store.Get(message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == "" || got.Recipient != "build@mail.test" {
		t.Fatalf("unexpected message: %#v", got)
	}
}

func TestCleanupRemovesExpiredMessage(t *testing.T) {
	store := testStore(t, time.Nanosecond)
	saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Subject: gone\r\n\r\nbody"))
	time.Sleep(time.Millisecond)
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	list, err := store.List("build@mail.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected expired message removal, got %#v", list)
	}
}

func TestSaveDeduplicatesMultiRecipientMessage(t *testing.T) {
	dataDir := t.TempDir()
	msgDir := filepath.Join(dataDir, "messages")
	store, err := OpenStore(filepath.Join(dataDir, "mail.db"), msgDir, Config{MailDomain: "mail.test", MessageTTL: time.Hour, MaxMessageBytes: 1024 * 1024, MaxStorageBytes: 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	messages, err := store.Save([]string{"first@mail.test", "second@mail.test"}, "sender@example.org", []byte("Subject: shared\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID == messages[1].ID {
		t.Fatalf("expected two distinct message records, got %#v", messages)
	}

	assertFileCount := func(want int) {
		t.Helper()
		entries, err := os.ReadDir(msgDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != want {
			t.Fatalf("expected %d stored file(s), got %v", want, entries)
		}
	}
	assertRefCount := func(want int) {
		t.Helper()
		var refCount int
		if err := store.db.QueryRow("SELECT ref_count FROM blobs").Scan(&refCount); err != nil {
			t.Fatal(err)
		}
		if refCount != want {
			t.Fatalf("expected blob ref_count %d, got %d", want, refCount)
		}
	}
	assertFileCount(1)
	assertRefCount(2)

	// Expiring one recipient's copy must not remove the shared file while the
	// other recipient's copy is still live.
	if _, err := store.db.Exec("UPDATE messages SET expires_at = 0 WHERE id = ?", messages[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	assertFileCount(1)
	assertRefCount(1)
	if _, err := store.Get(messages[1].ID); err != nil {
		t.Fatalf("expected surviving recipient's message to still be readable: %v", err)
	}

	// Expiring the last recipient's copy must remove the file.
	if _, err := store.db.Exec("UPDATE messages SET expires_at = 0 WHERE id = ?", messages[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	assertFileCount(0)
}

func TestOpenStoreMigratesLegacyPathSchema(t *testing.T) {
	dataDir := t.TempDir()
	msgDir := filepath.Join(dataDir, "messages")
	if err := os.MkdirAll(msgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "mail.db")

	// Build a legacy (pre-dedup) database by hand: one row per message, each
	// with its own path column, no blobs table.
	legacyDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY, recipient TEXT NOT NULL, sender TEXT NOT NULL,
		subject TEXT NOT NULL, received_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
		size INTEGER NOT NULL, path TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	type legacyRow struct {
		id, recipient, path, body string
	}
	rows := []legacyRow{
		{id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", recipient: "first@mail.test", path: filepath.Join(msgDir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.eml"), body: "Subject: one\r\n\r\nbody one"},
		{id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", recipient: "second@mail.test", path: filepath.Join(msgDir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.eml"), body: "Subject: two\r\n\r\nbody two"},
	}
	now := time.Now().Unix()
	for _, row := range rows {
		if err := os.WriteFile(row.path, []byte(row.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := legacyDB.Exec(`INSERT INTO messages (id, recipient, sender, subject, received_at, expires_at, size, path) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.recipient, "sender@example.org", "legacy", now, now+3600, int64(len(row.body)), row.path); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := Config{MailDomain: "mail.test", MessageTTL: time.Hour, MaxMessageBytes: 1024 * 1024, MaxStorageBytes: 1024 * 1024}
	store, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	for _, row := range rows {
		got, err := store.Get(row.id)
		if err != nil {
			t.Fatalf("expected migrated message %s to still be readable: %v", row.id, err)
		}
		if got.Recipient != row.recipient || got.Body != row.body {
			t.Fatalf("unexpected migrated message: %#v", got)
		}
		var blobID string
		if err := store.db.QueryRow("SELECT blob_id FROM messages WHERE id = ?", row.id).Scan(&blobID); err != nil {
			t.Fatal(err)
		}
		var blobPath string
		var refCount int
		if err := store.db.QueryRow("SELECT path, ref_count FROM blobs WHERE id = ?", blobID).Scan(&blobPath, &refCount); err != nil {
			t.Fatal(err)
		}
		if refCount != 1 || blobPath != row.path {
			t.Fatalf("expected a 1:1 migrated blob for %s, got path=%q ref_count=%d", row.id, blobPath, refCount)
		}
	}
	if _, err := os.Stat(rows[0].path); err != nil {
		t.Fatalf("expected migrated file to be left in place: %v", err)
	}

	// Reopening an already-migrated database must be a no-op.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Get(rows[0].id); err != nil {
		t.Fatalf("expected message to survive reopening an already-migrated database: %v", err)
	}
}

func TestListPageReturnsHasMore(t *testing.T) {
	store := testStore(t, time.Hour)
	for i := 0; i < 3; i++ {
		saveOne(t, store, "build@mail.test", "sender@example.org", []byte("Subject: page\r\n\r\nbody"))
	}

	first, hasMore, err := store.ListPage("build@mail.test", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !hasMore {
		t.Fatalf("expected two messages and another page, got len=%d hasMore=%t", len(first), hasMore)
	}
	second, hasMore, err := store.ListPage("build@mail.test", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || hasMore {
		t.Fatalf("expected final single message, got len=%d hasMore=%t", len(second), hasMore)
	}
}
