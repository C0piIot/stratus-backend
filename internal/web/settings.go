package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/settings"
)

// The settings page (#397): what can be changed while the server runs, where
// each value comes from, and a form that changes the ones the environment
// leaves to it.
//
// **One form and a plain POST**, like the rest of this UI. An empty field is
// the default -- the field says what that is -- so going back to it is
// clearing the field rather than a button per setting. A setting the
// environment fixes is shown and not offered: the decision is that the
// environment wins, and a field that took a value it would then ignore would
// be the page lying.
//
// **All or nothing.** Every value is read before any is stored, with the
// parser the environment's value goes through, so a form with one mistake in
// it changes nothing and comes back with the mistake marked.
const settingsPath = "/settings"

// SettingsEditor is what the page reads and writes: internal/settings's
// service, so a change is the one every loop and page then reads.
type SettingsEditor interface {
	Entries() []settings.Entry
	Set(ctx context.Context, key, raw string) error
	Reset(ctx context.Context, key string) error
}

// settingsSection is one heading of the page and the settings under it.
type settingsSection struct {
	Name   string
	Fields []settingsField
}

type settingsField struct {
	Key, Help, Default string
	// Value is what goes in the field: the stored value, or empty for the
	// default -- and for a setting the environment fixes, its value there.
	Value   string
	Choices []string
	Fixed   bool
	Error   string
}

func (h *handler) settingsPage(w http.ResponseWriter, r *http.Request, user string) {
	h.renderSettings(w, http.StatusOK, user, nil, nil, r.URL.Query().Has("saved"))
}

func (h *handler) saveSettings(w http.ResponseWriter, r *http.Request, user string) {
	if err := r.ParseForm(); err != nil {
		h.badRequest(w, user, "That form could not be read.")
		return
	}

	// What changes, read through the same parser the environment's value goes
	// through, before anything is stored.
	type change struct{ key, raw string }
	var changes []change
	posted := map[string]string{}
	problems := map[string]string{}
	for _, e := range h.editor.Entries() {
		if e.Source == settings.FromEnvironment || !r.PostForm.Has(e.Key) {
			continue
		}
		raw := r.PostForm.Get(e.Key)
		posted[e.Key] = raw
		current := ""
		if e.Source == settings.FromStore {
			current = e.Value
		}
		if raw == current {
			continue
		}
		var scratch config.Values
		if err := e.Apply(&scratch, raw); err != nil {
			problems[e.Key] = err.Error()
			continue
		}
		changes = append(changes, change{e.Key, raw})
	}
	if len(problems) > 0 {
		h.renderSettings(w, http.StatusBadRequest, user, posted, problems, false)
		return
	}

	for _, c := range changes {
		var err error
		if c.raw == "" {
			err = h.editor.Reset(r.Context(), c.key)
		} else {
			err = h.editor.Set(r.Context(), c.key, c.raw)
		}
		if err != nil {
			h.fail(w, r, user, fmt.Errorf("store %s: %w", c.key, err))
			return
		}
	}
	redirectLocal(w, r, settingsPath+"?saved")
}

// importSection is the heading the import folder's interval is under, where
// its path is shown beside it.
const importSection = "Import folder"

// importDirField is the import folder's path, shown and not offered: a path on
// this machine whose contents are moved and deleted is a decision about the
// deployment, and stays the environment's (#397).
func (h *handler) importDirField() settingsField {
	f := settingsField{
		Key: "STRATUS_INCOMING_DIR", Fixed: true,
		Help: "The folder on this machine whose contents are moved into the library; unset, there is none.",
	}
	if h.imports != nil {
		f.Value = h.imports.Dir()
	}
	return f
}

// renderSettings draws the page, with what was posted and what was wrong with
// it when a form came back refused.
func (h *handler) renderSettings(w http.ResponseWriter, code int, user string,
	posted, problems map[string]string, saved bool,
) {
	var sections []settingsSection
	for _, e := range h.editor.Entries() {
		f := settingsField{
			Key: e.Key, Help: e.Help, Default: e.Default, Choices: e.Choices,
			Fixed: e.Source == settings.FromEnvironment, Error: problems[e.Key],
		}
		switch {
		case f.Fixed:
			f.Value = e.Value
		case posted != nil:
			f.Value = posted[e.Key]
		case e.Source == settings.FromStore:
			f.Value = e.Value
		}
		if len(sections) == 0 || sections[len(sections)-1].Name != e.Section {
			sections = append(sections, settingsSection{Name: e.Section})
			if e.Section == importSection {
				sections[len(sections)-1].Fields = append(sections[len(sections)-1].Fields, h.importDirField())
			}
		}
		last := &sections[len(sections)-1]
		last.Fields = append(last.Fields, f)
	}
	v := view{Title: "Settings", User: user, Settings: sections}
	if saved {
		v.Notice = "Saved. It is in force now."
	}
	if len(problems) > 0 {
		v.Error = "Nothing was saved: one of these is not a value it takes."
	}
	h.render(w, code, pageSettings, v)
}
