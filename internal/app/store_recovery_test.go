package app

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveEvictionFailureRollsBackIncoming(t *testing.T) {
	s := testStore(t, time.Hour)
	raw := []byte("Subject: rollback\r\n\r\nbody")
	s.cfg.MaxStorageBytes = int64(len(raw))
	old := saveOne(t, s, "old@mail.test", "sender", raw)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON messages BEGIN SELECT RAISE(FAIL, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save([]string{"new@mail.test"}, "sender", raw); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := s.Get(old.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.List("new@mail.test")
	if err != nil || len(list) != 0 {
		t.Fatalf("failed save committed incoming: %v %v", list, err)
	}
	if s.storedMessages != 1 || s.storedBytes != int64(len(raw)) {
		t.Fatalf("wrong counters %d %d", s.storedMessages, s.storedBytes)
	}
	entries, _ := os.ReadDir(s.msgDir)
	if len(entries) != 1 {
		t.Fatalf("rollback leaked files: %v", entries)
	}
}

func TestCleanupFailureAfterCommittedBatchKeepsCounters(t *testing.T) {
	s := testStore(t, time.Hour)
	recipients := make([]string, cleanupBatchSize+1)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("r%d@mail.test", i)
	}
	raw := []byte("Subject: batched\r\n\r\nbody")
	if _, err := s.Save(recipients, "sender", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE messages SET expires_at = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_last BEFORE DELETE ON messages WHEN OLD.recipient = 'r%d@mail.test' BEGIN SELECT RAISE(FAIL, 'injected'); END`, cleanupBatchSize)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err == nil {
		t.Fatal("expected later-batch failure")
	}
	var count int
	_ = s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count)
	if count != 1 || s.storedMessages != 1 || s.storedBytes != int64(len(raw)) {
		t.Fatalf("wrong accounting db=%d cached=%d bytes=%d", count, s.storedMessages, s.storedBytes)
	}
	var refs int
	_ = s.db.QueryRow("SELECT ref_count FROM blobs").Scan(&refs)
	if refs != 1 {
		t.Fatalf("refs=%d", refs)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_last"); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if s.storedMessages != 0 || s.storedBytes != 0 {
		t.Fatalf("unreclaimed usage %d %d", s.storedMessages, s.storedBytes)
	}
}

func TestUnlinkFailureIsChargedAndRetried(t *testing.T) {
	s := testStore(t, time.Hour)
	raw := []byte("Subject: disk\r\n\r\nbody")
	s.cfg.MaxStorageBytes = int64(len(raw))
	old := saveOne(t, s, "old@mail.test", "sender", raw)
	s.removeFile = func(string) error { return errors.New("injected unlink failure") }
	incoming, err := s.Save([]string{"new@mail.test"}, "sender", raw)
	if err != nil {
		t.Fatalf("durable acceptance became error: %v", err)
	}
	if _, err := s.Get(incoming[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(old.ID); err != sql.ErrNoRows {
		t.Fatalf("old delivery retained: %v", err)
	}
	if s.storedBytes != 2*int64(len(raw)) {
		t.Fatalf("pending bytes lost: %d", s.storedBytes)
	}
	if _, err := s.Save([]string{"blocked@mail.test"}, "sender", raw); err == nil {
		t.Fatal("pending disk usage must block admission")
	}
	entries, _ := os.ReadDir(s.msgDir)
	if len(entries) != 2 {
		t.Fatalf("blocked admission leaked another file: %v", entries)
	}
	s.removeFile = os.Remove
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if s.storedBytes != int64(len(raw)) {
		t.Fatalf("retry usage=%d", s.storedBytes)
	}
	var pending int
	_ = s.db.QueryRow("SELECT count(*) FROM pending_deletions").Scan(&pending)
	if pending != 0 {
		t.Fatalf("pending=%d", pending)
	}
}

func TestStartupReconcilesUncommittedAndPendingFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mail.db")
	msgDir := filepath.Join(dir, "messages")
	cfg := Config{MessageTTL: time.Hour, MaxMessageBytes: 1024, MaxStorageBytes: 1024}
	s, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: recovery\r\n\r\nbody")
	m := saveOne(t, s, "Local@mail.test", "sender", raw)
	if _, err := s.db.Exec("UPDATE messages SET recipient = 'Local@MAIL.TEST' WHERE id = ?", m.ID); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(msgDir, "stale.eml")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO pending_deletions VALUES (?, ?)", stale, 5); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"orphan.eml", ".incoming-crash"} {
		if err := os.WriteFile(filepath.Join(msgDir, name), []byte("orphan"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, _ := os.ReadDir(msgDir)
	if len(entries) != 1 {
		t.Fatalf("unreconciled files: %v", entries)
	}
	if s.storedBytes != int64(len(raw)) || s.storedMessages != 1 {
		t.Fatalf("wrong recovery accounting")
	}
	list, err := s.List("Local@MAIL.TEST")
	if err != nil || len(list) != 1 || list[0].Recipient != "Local@mail.test" {
		t.Fatalf("legacy normalization: %v %v", list, err)
	}
}

func TestStartupReportsMissingAcceptedBlob(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mail.db")
	msgDir := filepath.Join(dir, "messages")
	cfg := Config{MessageTTL: time.Hour, MaxMessageBytes: 1024, MaxStorageBytes: 1024}
	s, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	saveOne(t, s, "r@mail.test", "sender", []byte("Subject: missing\r\n\r\nbody"))
	entries, _ := os.ReadDir(msgDir)
	if err := os.Remove(filepath.Join(msgDir, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenStore(dbPath, msgDir, cfg)
	if err == nil {
		reopened.Close()
		t.Fatal("missing accepted mail must be surfaced")
	}
	if !strings.Contains(err.Error(), "referenced blob") {
		t.Fatal(err)
	}
}

func TestEvictionQueryUsesReceivedIndex(t *testing.T) {
	s := testStore(t, time.Hour)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT id, blob_id FROM messages WHERE blob_id != ? ORDER BY received_at, rowid LIMIT 1`, "protected")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "messages_received") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("unindexed eviction: %s", plan)
	}
}

func TestStoreNormalizesRecipientDomainsWithoutFoldingLocalPart(t *testing.T) {
	s := testStore(t, time.Hour)
	saved, err := s.Save([]string{"Local@MAIL.TEST", "Local@mail.test", "local@mail.test"}, "sender", []byte("Subject: domains\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 {
		t.Fatalf("dedup recipients: %v", saved)
	}
	list, err := s.List("Local@MAIL.TEST")
	if err != nil || len(list) != 1 || list[0].Recipient != "Local@mail.test" {
		t.Fatalf("normalized list: %v %v", list, err)
	}
}

func TestRepeatedSavesDoNotAccumulateFailedUnlinks(t *testing.T) {
	s := testStore(t, time.Hour)
	raw := []byte("Subject: bounded\r\n\r\nbody")
	s.cfg.MaxStorageBytes = 3 * int64(len(raw))
	for i := 0; i < 3; i++ {
		saveOne(t, s, "old@mail.test", "sender", raw)
	}
	s.removeFile = func(string) error { return errors.New("injected unlink failure") }
	if _, err := s.Save([]string{"new@mail.test"}, "sender", raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.Save([]string{"blocked@mail.test"}, "sender", raw); err == nil {
			t.Fatal("accepted above cap with pending deletion")
		}
	}
	entries, _ := os.ReadDir(s.msgDir)
	if len(entries) != 4 || s.storedBytes != 4*int64(len(raw)) {
		t.Fatalf("unlink failures accumulate: files=%d bytes=%d", len(entries), s.storedBytes)
	}
}

func TestStorageLimitEvictsExpiredBeforeOlderLive(t *testing.T) {
	s := testStore(t, time.Hour)
	raw := []byte("Subject: expiry\r\n\r\nbody")
	live := saveOne(t, s, "live@mail.test", "sender", raw)
	expired := saveOne(t, s, "expired@mail.test", "sender", raw)
	if _, err := s.db.Exec("UPDATE messages SET expires_at = 0, received_at = ? WHERE id = ?", time.Now().Add(time.Hour).Unix(), expired.ID); err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxStorageBytes = 2 * int64(len(raw))
	if _, err := s.Save([]string{"incoming@mail.test"}, "sender", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(live.ID); err != nil {
		t.Fatalf("live message evicted before expired: %v", err)
	}
	var remaining int
	_ = s.db.QueryRow("SELECT count(*) FROM messages WHERE id = ?", expired.ID).Scan(&remaining)
	if remaining != 0 {
		t.Fatal("expired message retained")
	}
}

func TestPendingRecordSurvivesFailureAfterUnlink(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mail.db")
	msgDir := filepath.Join(dir, "messages")
	cfg := Config{MessageTTL: time.Hour, MaxMessageBytes: 1024, MaxStorageBytes: 1024}
	s, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: queue\r\n\r\nbody")
	saveOne(t, s, "r@mail.test", "sender", raw)
	if _, err := s.db.Exec("UPDATE messages SET expires_at = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_queue_delete BEFORE DELETE ON pending_deletions BEGIN SELECT RAISE(FAIL, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err == nil {
		t.Fatal("expected queue finalization failure")
	}
	entries, _ := os.ReadDir(msgDir)
	if len(entries) != 0 {
		t.Fatalf("expected unlink before failure: %v", entries)
	}
	if s.storedMessages != 0 || s.storedBytes != int64(len(raw)) {
		t.Fatalf("lost conservative accounting: messages=%d bytes=%d", s.storedMessages, s.storedBytes)
	}
	s.Close()
	s, err = OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.storedBytes != int64(len(raw)) {
		t.Fatalf("restart lost pending record: %d", s.storedBytes)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_queue_delete"); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if s.storedBytes != 0 {
		t.Fatalf("missing-file retry did not finalize queue: %d", s.storedBytes)
	}
}

func TestPendingRetriesAreBoundedWithoutStarvingLaterFiles(t *testing.T) {
	s := testStore(t, time.Hour)
	for i := 0; i < cleanupBatchSize+1; i++ {
		path := filepath.Join(s.msgDir, fmt.Sprintf("%04d.eml", i))
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO pending_deletions(path, size) VALUES (?, 1)", path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.loadStorageUsage(); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	s.removeFile = func(path string) error {
		attempts++
		if filepath.Base(path) != fmt.Sprintf("%04d.eml", cleanupBatchSize) {
			return errors.New("injected persistent failure")
		}
		return os.Remove(path)
	}
	if err := s.Cleanup(); err == nil {
		t.Fatal("expected failed first batch")
	}
	if attempts != cleanupBatchSize {
		t.Fatalf("unbounded first pass: %d", attempts)
	}
	if err := s.Cleanup(); err != nil {
		t.Fatalf("later file starved: %v", err)
	}
	if attempts != cleanupBatchSize+1 || s.storedBytes != cleanupBatchSize {
		t.Fatalf("retry accounting: attempts=%d bytes=%d", attempts, s.storedBytes)
	}
}

func TestUnrecordableOrphanBlocksAdmissionUntilReconciliation(t *testing.T) {
	dir := t.TempDir()
	dbPath, msgDir := filepath.Join(dir, "mail.db"), filepath.Join(dir, "messages")
	cfg := Config{MessageTTL: time.Hour, MaxMessageBytes: 1024, MaxStorageBytes: 1024}
	s, err := OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	raw := []byte("Subject: orphan failure\r\n\r\nbody")
	saveOne(t, s, "existing@mail.test", "sender", raw)
	for _, statement := range []string{
		`CREATE TRIGGER reject_insert BEFORE INSERT ON blobs BEGIN SELECT RAISE(ABORT, 'injected save failure'); END`,
		`CREATE TRIGGER reject_pending BEFORE INSERT ON pending_deletions BEGIN SELECT RAISE(ABORT, 'injected recovery failure'); END`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	s.removeFile = func(string) error { return errors.New("injected unlink failure") }
	if _, err := s.Save([]string{"failed@mail.test"}, "sender", raw); err == nil {
		t.Fatal("expected failed save")
	}
	if s.storageFault == nil || s.storedBytes != 2*int64(len(raw)) {
		t.Fatalf("unrecorded orphan was not charged: fault=%v bytes=%d", s.storageFault, s.storedBytes)
	}
	for _, statement := range []string{"DROP TRIGGER reject_insert", "DROP TRIGGER reject_pending"} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	s.removeFile = os.Remove
	if _, err := s.Save([]string{"blocked@mail.test"}, "sender", raw); err == nil {
		t.Fatal("admission resumed before orphan reconciliation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dbPath, msgDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.storageFault != nil || s.storedBytes != int64(len(raw)) {
		t.Fatalf("restart did not reconcile: fault=%v bytes=%d", s.storageFault, s.storedBytes)
	}
	if _, err := s.Save([]string{"recovered@mail.test"}, "sender", raw); err != nil {
		t.Fatalf("admission still blocked after reconciliation: %v", err)
	}
}
