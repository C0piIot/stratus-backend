package web

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-vcard"
	"golang.org/x/text/unicode/norm"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
)

// Events and contacts in the search box: matched here, in Go, over every
// object the services list, rather than by an index in each engine. A
// personal calendar and address book are hundreds of objects, which is what
// the CalDAV query by date already loads whole for the same reason, and an
// index would be three migrations and three engines' tokenisers for a few
// milliseconds.
//
// **The promise is db.Finder's, and one more**: whole words, several as a
// phrase, case ignored -- and accents too, since nothing but Go is doing the
// comparing and a contact called Amélie is typed as "amelie".

// searchCollectionLimit is how many events or contacts the page shows beside
// the other buckets; the page narrowed to one shows all of them, since every
// one of them was read to find the first.
const searchCollectionLimit = 20

type foundEvent struct {
	Href, Summary, When, Calendar string
	Repeats                       bool
	start                         time.Time
}

type foundContact struct {
	Href, Name, Detail string
}

// words is text as a search compares it: lower case, without the marks a
// letter carries once it is decomposed -- so é is e -- and split wherever a
// letter or a digit is not. Not a transform.Chain held in a variable: a chain
// keeps state between calls, and two searches at once tore it.
func words(text string) []string {
	var folded strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(text)) {
		if !unicode.Is(unicode.Mn, r) {
			folded.WriteRune(r)
		}
	}
	return strings.FieldsFunc(folded.String(), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// matches is whether any of texts holds the term's words, in order and
// side by side.
func matches(term []string, texts ...string) bool {
	if len(term) == 0 {
		return false
	}
	for _, text := range texts {
		w := words(text)
		for i := 0; i+len(term) <= len(w); i++ {
			if slices.Equal(w[i:i+len(term)], term) {
				return true
			}
		}
	}
	return false
}

// findEvents is every event of every calendar whose title, place or
// description holds the term, newest first: a repeating event once, by the
// event its instances come from.
func (h *handler) findEvents(ctx context.Context, user, term string) []foundEvent {
	if h.calendars == nil {
		return nil
	}
	want := words(term)
	all, err := h.calendars.Calendars(ctx, user)
	if err != nil {
		slog.WarnContext(ctx, "listing calendars for a search", "err", err)
		return nil
	}
	zone := h.zone()
	var out []foundEvent
	for _, c := range all {
		objects, err := h.calendars.Objects(ctx, user, c.Name)
		if err != nil {
			slog.WarnContext(ctx, "reading a calendar for a search", "calendar", c.Name, "err", err)
			continue
		}
		for _, o := range objects {
			if o.Component != ical.CompEvent {
				continue
			}
			cal, err := calendar.Decode(o)
			if err != nil {
				continue
			}
			ev := master(cal)
			if ev == nil {
				continue
			}
			text := func(k string) string { v, _ := ev.Props.Text(k); return v }
			if !matches(want, text(ical.PropSummary), text(ical.PropLocation), text(ical.PropDescription)) {
				continue
			}
			row := foundEvent{
				Href: eventHref(eventEditPrefix, c.Name, o.Name), Summary: text(ical.PropSummary),
				Calendar: displayName(c), Repeats: repeats(ev) != "",
			}
			if row.Summary == "" {
				row.Summary = "(no title)"
			}
			if ev.Props.Get(ical.PropDateTimeStart) != nil {
				if occ, err := calendar.When(ev, zone); err == nil {
					row.start = occ.Start
					row.When = occ.Start.Format("Mon 2 Jan 2006")
					if !occ.AllDay {
						row.When += ", " + occ.Start.Format("15:04")
					}
				}
			}
			out = append(out, row)
		}
	}
	slices.SortStableFunc(out, func(a, b foundEvent) int { return b.start.Compare(a.start) })
	return out
}

// findContacts is every contact whose name, company, email or note holds
// the term, by name.
func (h *handler) findContacts(ctx context.Context, user, term string) []foundContact {
	if h.contacts == nil {
		return nil
	}
	want := words(term)
	books, err := h.contacts.AddressBooks(ctx, user)
	if err != nil {
		slog.WarnContext(ctx, "listing address books for a search", "err", err)
		return nil
	}
	var out []foundContact
	for _, b := range books {
		list, err := h.contacts.Contacts(ctx, user, b.Name)
		if err != nil {
			slog.WarnContext(ctx, "reading an address book for a search", "book", b.Name, "err", err)
			continue
		}
		for _, c := range list {
			card, err := contacts.Decode(c)
			if err != nil {
				continue
			}
			name := card.PreferredValue(vcard.FieldFormattedName)
			org := strings.Split(card.PreferredValue(vcard.FieldOrganization), ";")[0]
			texts := []string{name, org, card.PreferredValue(vcard.FieldNote)}
			for _, e := range card[vcard.FieldEmail] {
				texts = append(texts, e.Value)
			}
			if !matches(want, texts...) {
				continue
			}
			detail := org
			if email := card.PreferredValue(vcard.FieldEmail); email != "" {
				detail = strings.TrimPrefix(strings.Join([]string{org, email}, " · "), " · ")
			}
			out = append(out, foundContact{Href: contactHref(contactEditPrefix, b.Name, c.Name), Name: name, Detail: detail})
		}
	}
	slices.SortStableFunc(out, func(a, b foundContact) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}
