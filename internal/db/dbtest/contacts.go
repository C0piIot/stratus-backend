package dbtest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// RunContacts executes the address book cases against the repository built by
// newRepo. They are the calendar cases again, because the two are the same
// shape and a driver may not get one of them right and the other wrong.
func RunContacts(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"an address book comes back as it was stored", addressBookRoundTrip},
		{"an owner cannot have two address books by one name", addressBookNameTaken},
		{"owners do not see each other's address books", addressBookOwnersAreSeparate},
		{"a missing address book is ErrNotFound", addressBookMissing},
		{"a contact comes back as it was stored", contactRoundTrip},
		{"a contact with a photograph is stored whole", contactLarge},
		{"writing a name again replaces it and moves the token", contactReplace},
		{"one UID cannot be two contacts in an address book", contactUIDTaken},
		{"one UID can be in two address books", contactUIDPerBook},
		{"a missing contact is ErrNotFound", contactMissing},
		{"changes since a token are writes and deletions in order", contactChanges},
		{"pruning forgets old deletions and says how far", contactPrune},
		{"an address book is renamed and described", addressBookUpdate},
		{"deleting an address book deletes what it held and remembered", addressBookDelete},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

func newAddressBook(t *testing.T, s db.Repo, ownerID, name string) db.AddressBook {
	t.Helper()
	b, err := s.CreateAddressBook(t.Context(), db.AddressBook{
		OwnerID: ownerID, Name: name, DisplayName: "Shown " + name, Description: "About " + name, Created: calendarAt,
	})
	if err != nil {
		t.Fatalf("CreateAddressBook(%q): %v", name, err)
	}
	return b
}

func vcard(uid string) string {
	return "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Ada Lovelace\r\nEND:VCARD\r\n"
}

func putContact(t *testing.T, s db.Repo, b db.AddressBook, name, uid string) db.Contact {
	t.Helper()
	c, err := s.PutContact(t.Context(), db.Contact{
		AddressBookID: b.ID, Name: name, UID: uid, Data: vcard(uid), ETag: `"` + uid + `"`, Changed: calendarAt,
	})
	if err != nil {
		t.Fatalf("PutContact(%q): %v", name, err)
	}
	return c
}

func addressBookByName(t *testing.T, s db.Repo, b db.AddressBook) db.AddressBook {
	t.Helper()
	got, err := s.AddressBookByName(t.Context(), b.OwnerID, b.Name)
	if err != nil {
		t.Fatalf("AddressBookByName(%q): %v", b.Name, err)
	}
	return got
}

func addressBookRoundTrip(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	if b.ID == 0 {
		t.Fatal("CreateAddressBook returned no id")
	}
	want := db.AddressBook{
		ID: b.ID, OwnerID: "alice", Name: "work", DisplayName: "Shown work", Description: "About work",
		Created: calendarAt.Truncate(time.Millisecond),
	}
	if got := addressBookByName(t, s, b); got != want {
		t.Errorf("AddressBookByName = %+v, want %+v", got, want)
	}

	newAddressBook(t, s, "alice", "home")
	list, err := s.AddressBooks(t.Context(), "alice")
	if err != nil {
		t.Fatalf("AddressBooks: %v", err)
	}
	if len(list) != 2 || list[0].Name != "home" || list[1].Name != "work" {
		t.Errorf("AddressBooks = %+v, want home then work", list)
	}
}

func addressBookNameTaken(t *testing.T, s db.Repo) {
	newAddressBook(t, s, "alice", "work")
	_, err := s.CreateAddressBook(t.Context(), db.AddressBook{OwnerID: "alice", Name: "work", Created: calendarAt})
	if !errors.Is(err, db.ErrConflict) {
		t.Errorf("second CreateAddressBook = %v, want ErrConflict", err)
	}
}

func addressBookOwnersAreSeparate(t *testing.T, s db.Repo) {
	newAddressBook(t, s, "alice", "work")
	newAddressBook(t, s, "bob", "work")

	if _, err := s.AddressBookByName(t.Context(), "carol", "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("AddressBookByName for another owner = %v, want ErrNotFound", err)
	}
	list, err := s.AddressBooks(t.Context(), "bob")
	if err != nil {
		t.Fatalf("AddressBooks: %v", err)
	}
	if len(list) != 1 || list[0].OwnerID != "bob" {
		t.Errorf("AddressBooks(bob) = %+v, want bob's one", list)
	}
}

func addressBookMissing(t *testing.T, s db.Repo) {
	if _, err := s.AddressBookByName(t.Context(), "alice", "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("AddressBookByName = %v, want ErrNotFound", err)
	}
	_, err := s.PutContact(t.Context(), db.Contact{
		AddressBookID: 9999, Name: "a.vcf", UID: "a", Data: vcard("a"), Changed: calendarAt,
	})
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PutContact into no address book = %v, want ErrNotFound", err)
	}
}

func contactRoundTrip(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	put := putContact(t, s, b, "ada.vcf", "ada@example.com")
	if put.SyncToken == 0 {
		t.Error("PutContact returned no token")
	}

	want := db.Contact{
		AddressBookID: b.ID, Name: "ada.vcf", UID: "ada@example.com", Data: vcard("ada@example.com"),
		ETag: `"ada@example.com"`, SyncToken: put.SyncToken, Changed: calendarAt.Truncate(time.Millisecond),
	}
	got, err := s.Contact(t.Context(), b.ID, "ada.vcf")
	if err != nil {
		t.Fatalf("Contact: %v", err)
	}
	if got != want || put != want {
		t.Errorf("Contact = %+v, PutContact = %+v, want %+v", got, put, want)
	}

	putContact(t, s, b, "alan.vcf", "alan@example.com")
	list, err := s.Contacts(t.Context(), b.ID)
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	if len(list) != 2 || list[0] != want || list[1].Name != "alan.vcf" {
		t.Errorf("Contacts = %+v, want %+v then alan.vcf", list, want)
	}
	if got := addressBookByName(t, s, b); got.SyncToken != 2 {
		t.Errorf("address book token = %d after two writes, want 2", got.SyncToken)
	}
}

// contactLarge is past the 64 KB a MySQL TEXT holds, which a photograph
// inside a vCard reaches.
func contactLarge(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	data := strings.Replace(vcard("big"), "FN:Ada Lovelace",
		"FN:Ada Lovelace\r\nPHOTO:data:image/jpeg;base64,"+strings.Repeat("QUJD", 50_000), 1)
	if _, err := s.PutContact(t.Context(), db.Contact{
		AddressBookID: b.ID, Name: "big.vcf", UID: "big", Data: data, Changed: calendarAt,
	}); err != nil {
		t.Fatalf("PutContact: %v", err)
	}
	got, err := s.Contact(t.Context(), b.ID, "big.vcf")
	if err != nil {
		t.Fatalf("Contact: %v", err)
	}
	if got.Data != data {
		t.Errorf("Data came back as %d bytes, want %d", len(got.Data), len(data))
	}
}

func contactReplace(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	first := putContact(t, s, b, "ada.vcf", "ada")

	second, err := s.PutContact(t.Context(), db.Contact{
		AddressBookID: b.ID, Name: "ada.vcf", UID: "ada", Data: vcard("ada") + " ", ETag: `"moved"`,
		Changed: calendarAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second PutContact: %v", err)
	}
	if second.SyncToken <= first.SyncToken {
		t.Errorf("token went from %d to %d, want it to grow", first.SyncToken, second.SyncToken)
	}
	list, err := s.Contacts(t.Context(), b.ID)
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	if len(list) != 1 || list[0].ETag != `"moved"` {
		t.Errorf("Contacts = %+v, want the second write alone", list)
	}
}

func contactUIDTaken(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	putContact(t, s, b, "one.vcf", "ada")

	_, err := s.PutContact(t.Context(), db.Contact{
		AddressBookID: b.ID, Name: "two.vcf", UID: "ada", Data: vcard("ada"), Changed: calendarAt,
	})
	if !errors.Is(err, db.ErrConflict) {
		t.Errorf("PutContact with a taken UID = %v, want ErrConflict", err)
	}
	if got, err := s.Contact(t.Context(), b.ID, "one.vcf"); err != nil || got.UID != "ada" {
		t.Errorf("the contact holding the UID = %+v, %v; want it untouched", got, err)
	}
	if _, err := s.Contact(t.Context(), b.ID, "two.vcf"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the refused contact = %v, want ErrNotFound", err)
	}
}

func contactUIDPerBook(t *testing.T, s db.Repo) {
	work := newAddressBook(t, s, "alice", "work")
	home := newAddressBook(t, s, "alice", "home")
	putContact(t, s, work, "ada.vcf", "ada")
	putContact(t, s, home, "ada.vcf", "ada")
}

func contactMissing(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	if _, err := s.Contact(t.Context(), b.ID, "nope.vcf"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Contact = %v, want ErrNotFound", err)
	}
	if err := s.DeleteContact(t.Context(), b.ID, "nope.vcf", calendarAt); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("DeleteContact = %v, want ErrNotFound", err)
	}
	if got := addressBookByName(t, s, b); got.SyncToken != 0 {
		t.Errorf("token = %d after deleting nothing, want 0", got.SyncToken)
	}
}

func contactChanges(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	other := newAddressBook(t, s, "alice", "home")
	putContact(t, s, b, "a.vcf", "a")
	putContact(t, s, b, "b.vcf", "b")
	putContact(t, s, other, "elsewhere.vcf", "elsewhere")
	if err := s.DeleteContact(t.Context(), b.ID, "a.vcf", calendarAt); err != nil {
		t.Fatalf("DeleteContact: %v", err)
	}
	putContact(t, s, b, "c.vcf", "c")

	got, err := s.ContactChanges(t.Context(), b.ID, 0)
	if err != nil {
		t.Fatalf("ContactChanges: %v", err)
	}
	want := []db.ContactChange{
		{Name: "b.vcf", SyncToken: 2},
		{Name: "a.vcf", SyncToken: 3, Deleted: true},
		{Name: "c.vcf", SyncToken: 4},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ContactChanges(0) = %+v, want %+v", got, want)
	}

	// Written again after a delete, a name is one change and not two.
	again := putContact(t, s, b, "a.vcf", "a")
	got, err = s.ContactChanges(t.Context(), b.ID, 2)
	if err != nil {
		t.Fatalf("ContactChanges: %v", err)
	}
	want = []db.ContactChange{{Name: "c.vcf", SyncToken: 4}, {Name: "a.vcf", SyncToken: again.SyncToken}}
	if !slices.Equal(got, want) {
		t.Errorf("ContactChanges(2) = %+v, want %+v", got, want)
	}
}

func contactPrune(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	untouched := newAddressBook(t, s, "alice", "home")
	for _, name := range []string{"old.vcf", "older.vcf", "new.vcf"} {
		putContact(t, s, b, name, name)
	}
	putContact(t, s, untouched, "kept.vcf", "kept")
	deleteAt := func(name string, at time.Time) {
		t.Helper()
		if err := s.DeleteContact(t.Context(), b.ID, name, at); err != nil {
			t.Fatalf("DeleteContact(%q): %v", name, err)
		}
	}
	deleteAt("older.vcf", calendarAt)
	deleteAt("old.vcf", calendarAt.Add(time.Hour))
	deleteAt("new.vcf", calendarAt.Add(48*time.Hour))

	n, err := s.PruneContactTombstones(t.Context(), calendarAt.Add(24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneContactTombstones = %d, %v; want 2", n, err)
	}
	if got := addressBookByName(t, s, b); got.PrunedToken != 5 {
		t.Errorf("PrunedToken = %d, want 5, the later of the two forgotten", got.PrunedToken)
	}
	if got := addressBookByName(t, s, untouched); got.PrunedToken != 0 {
		t.Errorf("PrunedToken of an address book with nothing pruned = %d, want 0", got.PrunedToken)
	}
	got, err := s.ContactChanges(t.Context(), b.ID, 0)
	if err != nil {
		t.Fatalf("ContactChanges: %v", err)
	}
	want := []db.ContactChange{{Name: "new.vcf", SyncToken: 6, Deleted: true}}
	if !slices.Equal(got, want) {
		t.Errorf("ContactChanges = %+v, want %+v", got, want)
	}
}

func addressBookUpdate(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	putContact(t, s, b, "a.vcf", "a")

	b.DisplayName, b.Description = "Office", ""
	if err := s.UpdateAddressBook(t.Context(), b); err != nil {
		t.Fatalf("UpdateAddressBook: %v", err)
	}
	got := addressBookByName(t, s, b)
	if got.DisplayName != "Office" || got.Description != "" || got.SyncToken != 1 {
		t.Errorf("after UpdateAddressBook = %+v, want the new names and the same token", got)
	}
	if err := s.UpdateAddressBook(t.Context(), db.AddressBook{OwnerID: "bob", Name: "work"}); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("UpdateAddressBook of another owner's = %v, want ErrNotFound", err)
	}
}

func addressBookDelete(t *testing.T, s db.Repo) {
	b := newAddressBook(t, s, "alice", "work")
	kept := newAddressBook(t, s, "alice", "home")
	putContact(t, s, b, "a.vcf", "a")
	putContact(t, s, b, "b.vcf", "b")
	putContact(t, s, kept, "a.vcf", "a")
	if err := s.DeleteContact(t.Context(), b.ID, "b.vcf", calendarAt); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAddressBook(t.Context(), "bob", "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("DeleteAddressBook of another owner's = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAddressBook(t.Context(), "alice", "work"); err != nil {
		t.Fatalf("DeleteAddressBook: %v", err)
	}
	if _, err := s.AddressBookByName(t.Context(), "alice", "work"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the deleted address book = %v, want ErrNotFound", err)
	}
	if got, err := s.ContactChanges(t.Context(), b.ID, 0); err != nil || len(got) != 0 {
		t.Errorf("what the deleted address book held = %+v, %v; want nothing", got, err)
	}
	if got, err := s.Contacts(t.Context(), kept.ID); err != nil || len(got) != 1 {
		t.Errorf("the other address book = %+v, %v; want its one contact", got, err)
	}
}
