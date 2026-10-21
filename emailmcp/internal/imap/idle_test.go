package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
)

func TestUIDSetAfter(t *testing.T) {
	if got := UIDSetAfter(10).String(); got != "11:*" {
		t.Fatalf("UIDSetAfter(10)=%q", got)
	}
	if got := UIDSetAfter(0).String(); got != "1:*" {
		t.Fatalf("UIDSetAfter(0)=%q", got)
	}
}

func TestSnapshotLastUID(t *testing.T) {
	if SnapshotLastUID(50) != 49 {
		t.Fatalf("expected 49")
	}
	if SnapshotLastUID(0) != 0 {
		t.Fatalf("empty mailbox snapshot should be 0")
	}
	if SnapshotLastUID(1) != 0 {
		t.Fatalf("UIDNEXT 1 snapshot should be 0")
	}
}

func TestResolvedUIDNext(t *testing.T) {
	got, err := ResolvedUIDNext(50, 0, 0, 3)
	if err != nil || got != 50 {
		t.Fatalf("select uidnext: got=%d err=%v", got, err)
	}
	got, err = ResolvedUIDNext(0, 40, 0, 3)
	if err != nil || got != 40 {
		t.Fatalf("status uidnext: got=%d err=%v", got, err)
	}
	got, err = ResolvedUIDNext(0, 0, 19, 3)
	if err != nil || got != 20 {
		t.Fatalf("max uid: got=%d err=%v", got, err)
	}
	got, err = ResolvedUIDNext(0, 0, 0, 0)
	if err != nil || got != 1 {
		t.Fatalf("empty mailbox: got=%d err=%v", got, err)
	}
	if _, err := ResolvedUIDNext(0, 0, 0, 5); err == nil {
		t.Fatal("expected error when uidnext cannot be resolved")
	}
}

func TestMailboxExistsUpdate(t *testing.T) {
	if mailboxExistsUpdate(nil) {
		t.Fatal("nil data should not wake")
	}
	if mailboxExistsUpdate(&imapclient.UnilateralDataMailbox{}) {
		t.Fatal("flags-only update should not wake")
	}
	n := uint32(3)
	if !mailboxExistsUpdate(&imapclient.UnilateralDataMailbox{NumMessages: &n}) {
		t.Fatal("EXISTS should wake")
	}
}

func TestNotifyExistsNonBlocking(t *testing.T) {
	ch := make(chan struct{}, 1)
	notifyExists(ch)
	notifyExists(ch) // must not block when already signaled
	select {
	case <-ch:
	default:
		t.Fatal("expected queued EXISTS")
	}
	select {
	case <-ch:
		t.Fatal("buffer should still be one")
	default:
	}
}

func TestExistsMapsToUIDRange(t *testing.T) {
	lastUID := SnapshotLastUID(50) // existing mail up to 49
	if lastUID != 49 {
		t.Fatalf("lastUID=%d", lastUID)
	}
	n := uint32(51)
	if !mailboxExistsUpdate(&imapclient.UnilateralDataMailbox{NumMessages: &n}) {
		t.Fatal("EXISTS should map to a fetch")
	}
	if got := UIDSetAfter(lastUID).String(); got != "50:*" {
		t.Fatalf("EXISTS UID range=%q", got)
	}
}
