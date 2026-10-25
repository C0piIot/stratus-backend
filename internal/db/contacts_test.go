package db_test

import (
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func TestAnAddressBookAndAContactAreStoredInUTCToTheMillisecond(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("CEST", 2*60*60)
	when := time.Date(2024, 6, 1, 12, 0, 0, 999_999_999, zone)
	want := time.Date(2024, 6, 1, 10, 0, 0, 999_000_000, time.UTC)

	if got := (db.AddressBook{Created: when}).Normalize().Created; !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("AddressBook.Normalize = %v, want %v", got, want)
	}
	if got := (db.Contact{Changed: when}).Normalize().Changed; !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("Contact.Normalize = %v, want %v", got, want)
	}
}
