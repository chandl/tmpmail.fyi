package app

import (
	"path/filepath"
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

func TestStoreSavesAndListsMessage(t *testing.T) {
	store := testStore(t, time.Hour)
	message, err := store.Save("build@mail.test", "sender@example.org", []byte("From: Sender <sender@example.org>\r\nSubject: Hello ✓\r\n\r\nThe body"))
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := store.Save("build@mail.test", "sender@example.org", []byte("Subject: gone\r\n\r\nbody")); err != nil {
		t.Fatal(err)
	}
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
