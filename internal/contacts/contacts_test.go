package contacts_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-vcard"

	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/sqlite"
)

const owner = "edu"

func service(t *testing.T) (*contacts.Service, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return contacts.New(store), store
}

func parse(t *testing.T, text string) vcard.Card {
	t.Helper()
	card, err := vcard.NewDecoder(strings.NewReader(strings.ReplaceAll(text, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return card
}

// ada is how a client sends one: properties in its own order, which does not
// survive being stored.
const ada = `BEGIN:VCARD
VERSION:4.0
UID:urn:uuid:ada
FN:Ada Lovelace
TEL;TYPE=cell:+44 20 7946 0000
EMAIL:ada@example.com
END:VCARD
`

func TestEveryOwnerHasAnAddressBook(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	list, err := s.AddressBooks(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != contacts.DefaultName {
		t.Fatalf("AddressBooks = %+v, want the default one", list)
	}
	again, err := s.AddressBooks(t.Context(), owner)
	if err != nil || len(again) != 1 || again[0].ID != list[0].ID {
		t.Errorf("a second look = %+v, %v; want the same one", again, err)
	}
}

func TestAContactIsStoredAsItIsServed(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	// Written before anything listed: the default address book is made then.
	put, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), nil)
	if err != nil {
		t.Fatal(err)
	}
	if put.UID != "urn:uuid:ada" || put.ETag == "" {
		t.Errorf("Put = %+v", put)
	}
	got, err := s.Contact(t.Context(), owner, contacts.DefaultName, "ada.vcf")
	if err != nil {
		t.Fatal(err)
	}
	card, err := contacts.Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := contacts.Encode(card)
	if err != nil || again != got.Data {
		t.Errorf("what is served is not what is stored:\n%s\n%s", again, got.Data)
	}
	list, err := s.Contacts(t.Context(), owner, contacts.DefaultName)
	if err != nil || len(list) != 1 {
		t.Errorf("Contacts = %+v, %v", list, err)
	}
}

func TestAContactNeedsAUIDAndAName(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	for name, text := range map[string]string{
		"no UID": strings.Replace(ada, "UID:urn:uuid:ada\n", "", 1),
		"no FN":  strings.Replace(ada, "FN:Ada Lovelace\n", "", 1),
	} {
		if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, text), nil); !errors.Is(err, contacts.ErrInvalid) {
			t.Errorf("%s: Put = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", nil, nil); !errors.Is(err, contacts.ErrInvalid) {
		t.Errorf("no card: Put = %v, want ErrInvalid", err)
	}
	noVersion := parse(t, ada)
	delete(noVersion, vcard.FieldVersion)
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", noVersion, nil); !errors.Is(err, contacts.ErrInvalid) {
		t.Errorf("no VERSION: Put = %v, want ErrInvalid", err)
	}
	for _, name := range []string{"", "a/b", ".."} {
		if _, err := s.Put(t.Context(), owner, contacts.DefaultName, name, parse(t, ada), nil); !errors.Is(err, db.ErrInvalidPath) {
			t.Errorf("name %q: Put = %v, want ErrInvalidPath", name, err)
		}
	}
}

func TestAPreconditionIsAskedInsideTheWrite(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	first, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), nil)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("precondition failed")
	var saw string
	_, err = s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), func(etag string, exists bool) error {
		saw = etag
		if !exists {
			t.Error("the contact was said not to exist")
		}
		return refused
	})
	if !errors.Is(err, refused) || saw != first.ETag {
		t.Errorf("Put = %v having seen %q, want the refusal having seen %q", err, saw, first.ETag)
	}
}

func TestOneUIDIsOneContact(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "other.vcf", parse(t, ada), nil); !errors.Is(err, db.ErrConflict) {
		t.Errorf("a second contact with the UID = %v, want ErrConflict", err)
	}
}

func TestChangesAreWhatWasWrittenAndDeleted(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), nil); err != nil {
		t.Fatal(err)
	}
	alan := strings.NewReplacer("urn:uuid:ada", "urn:uuid:alan", "Ada Lovelace", "Alan Turing").Replace(ada)
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "alan.vcf", parse(t, alan), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), owner, contacts.DefaultName, "ada.vcf"); err != nil {
		t.Fatal(err)
	}

	first, b, err := s.Changes(t.Context(), owner, contacts.DefaultName, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Name != "alan.vcf" || b.SyncToken != 3 {
		t.Errorf("a first sync = %+v at %d, want alan.vcf alone at 3", first, b.SyncToken)
	}
	later, _, err := s.Changes(t.Context(), owner, contacts.DefaultName, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 2 || later[1].Name != "ada.vcf" || !later[1].Deleted {
		t.Errorf("changes since 1 = %+v, want alan.vcf and ada.vcf deleted", later)
	}
	for _, stale := range []int64{-1, 4} {
		if _, _, err := s.Changes(t.Context(), owner, contacts.DefaultName, stale); !errors.Is(err, contacts.ErrStaleToken) {
			t.Errorf("Changes(%d) = %v, want ErrStaleToken", stale, err)
		}
	}
	if err := s.Delete(t.Context(), owner, contacts.DefaultName, "ada.vcf"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}
}

func TestAnAddressBookIsMadeNamedAndDeleted(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Create(t.Context(), owner, db.AddressBook{Name: "work", DisplayName: "Work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(t.Context(), owner, db.AddressBook{Name: "work"}); !errors.Is(err, db.ErrConflict) {
		t.Errorf("a second one by the name = %v, want ErrConflict", err)
	}
	if _, err := s.Create(t.Context(), owner, db.AddressBook{Name: "a/b"}); !errors.Is(err, db.ErrInvalidPath) {
		t.Errorf("a name with a slash = %v, want ErrInvalidPath", err)
	}

	shown, about := "Office", "Colleagues"
	got, err := s.Update(t.Context(), owner, "work", contacts.Patch{DisplayName: &shown, Description: &about})
	if err != nil || got.DisplayName != shown || got.Description != about {
		t.Errorf("Update = %+v, %v", got, err)
	}
	if _, err := s.Update(t.Context(), owner, "nope", contacts.Patch{DisplayName: &shown}); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Update of none = %v, want ErrNotFound", err)
	}

	if err := s.DeleteAddressBook(t.Context(), owner, contacts.DefaultName); !errors.Is(err, contacts.ErrDefault) {
		t.Errorf("deleting the default = %v, want ErrDefault", err)
	}
	if err := s.DeleteAddressBook(t.Context(), owner, "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddressBook(t.Context(), owner, "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the deleted one = %v, want ErrNotFound", err)
	}
	if _, err := s.Contacts(t.Context(), owner, "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("listing the deleted one = %v, want ErrNotFound", err)
	}
	if _, err := s.Contact(t.Context(), owner, "work", "a.vcf"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("reading from the deleted one = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Changes(t.Context(), owner, "work", 0); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("changes to the deleted one = %v, want ErrNotFound", err)
	}
}

func TestOldDeletionsArePruned(t *testing.T) {
	t.Parallel()
	s, store := service(t)
	if _, err := s.Put(t.Context(), owner, contacts.DefaultName, "ada.vcf", parse(t, ada), nil); err != nil {
		t.Fatal(err)
	}
	b, err := s.AddressBook(t.Context(), owner, contacts.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteContact(t.Context(), b.ID, "ada.vcf", time.Now().Add(-contacts.TombstoneRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(t.Context()); err != nil || n != 1 {
		t.Errorf("Prune = %d, %v; want 1", n, err)
	}
	if _, _, err := s.Changes(t.Context(), owner, contacts.DefaultName, 1); !errors.Is(err, contacts.ErrStaleToken) {
		t.Errorf("a token from before the pruning = %v, want ErrStaleToken", err)
	}
}

func TestADatabaseThatFailsSaysSo(t *testing.T) {
	t.Parallel()
	_, store := service(t)
	s := contacts.New(dbtest.FailOn(t, store, "PruneContactTombstones"))
	if _, err := s.Prune(t.Context()); err == nil {
		t.Error("Prune over a failing database succeeded")
	}
	if _, err := contacts.Decode(db.Contact{Name: "x.vcf", Data: "not a vcard"}); err == nil {
		t.Error("Decode took text that is not a vCard")
	}
}

// TestAContactIsAlwaysTheSameBytes is what the ETag rests on: go-vcard writes
// a field's parameters in map order, so a number with two of them came out
// either way round.
func TestAContactIsAlwaysTheSameBytes(t *testing.T) {
	t.Parallel()
	card := parse(t, "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:x\r\nFN:A\\, B\r\n"+
		"item1.TEL;TYPE=cell,voice;PREF=1;X-A=1;X-B=2:+1 555\r\nNOTE:one\\ntwo\r\nEND:VCARD\r\n")
	first, err := contacts.Encode(card)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if again, _ := contacts.Encode(card); again != first {
			t.Fatalf("the same card came out twice differently:\n%s\n%s", first, again)
		}
	}
	if want := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:A\\, B\r\nNOTE:one\\ntwo\r\n" +
		"item1.TEL;PREF=1;TYPE=cell;TYPE=voice;X-A=1;X-B=2:+1 555\r\nUID:x\r\nEND:VCARD\r\n"; first != want {
		t.Errorf("the card is\n%q, want\n%q", first, want)
	}
	if again, _ := contacts.Encode(parse(t, first)); again != first {
		t.Errorf("a card read back is not the same bytes:\n%s", again)
	}
	if _, err := contacts.Encode(vcard.Card{}); !errors.Is(err, contacts.ErrInvalid) {
		t.Errorf("a card with no VERSION = %v", err)
	}
}
