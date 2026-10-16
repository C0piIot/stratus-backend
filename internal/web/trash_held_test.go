package web_test

import (
	"strings"
	"testing"
)

// TestTheTrashCanBeHeld: with the retention at never, every page that used to
// say how many days a deletion has says that it stays, and the status page
// says the trash is held even while it is empty -- it is a setting somebody
// turned on for a while, and the reminder is the point (#296).
func TestTheTrashCanBeHeld(t *testing.T) {
	t.Parallel()
	h, s := heldBrowser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	status := get(t, h, "/status", cookie).Body.String()
	if !strings.Contains(status, "data-trash-held") {
		t.Error("the status page does not say the trash is held while it is empty")
	}

	form := get(t, h, "/delete/notes.txt", cookie).Body.String()
	if strings.Contains(form, " days") || !strings.Contains(form, "until you delete it") {
		t.Errorf("the delete page does not say the deletion stays:\n%s", form)
	}

	empty := get(t, h, "/trash/", cookie).Body.String()
	if strings.Contains(empty, " days") || !strings.Contains(empty, "stays until you delete it") {
		t.Errorf("the empty trash does not say a deletion stays:\n%s", empty)
	}

	if err := s.Remove(t.Context(), username, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	full := get(t, h, "/trash/", cookie).Body.String()
	if strings.Contains(full, " days") || !strings.Contains(full, "nothing in it runs out") {
		t.Errorf("the trash does not say it is held:\n%s", full)
	}
	if status := get(t, h, "/status", cookie).Body.String(); !strings.Contains(status, "data-trash-held") {
		t.Error("the status page does not say the trash is held")
	}
}

// TestTheTrashIsNotHeldByDefault is the other side: a deployment that set
// nothing is not told its trash is held.
func TestTheTrashIsNotHeldByDefault(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	if err := s.Remove(t.Context(), username, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	cookie := signIn(t, h)
	if status := get(t, h, "/status", cookie).Body.String(); strings.Contains(status, "data-trash-held") {
		t.Error("the status page says the trash is held with the default retention")
	}
	if trash := get(t, h, "/trash/", cookie).Body.String(); !strings.Contains(trash, "30 days") {
		t.Error("the trash does not say how long a deletion is kept")
	}
}
