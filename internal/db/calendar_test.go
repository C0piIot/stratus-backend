package db_test

import (
	"testing"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

func TestACalendarHoldsWhatItSays(t *testing.T) {
	t.Parallel()
	tasks := db.Calendar{Components: "VTODO"}
	all := db.Calendar{Components: "VEVENT,VTODO,VJOURNAL"}

	if !tasks.Holds("VTODO") || tasks.Holds("VEVENT") || tasks.Holds("TODO") {
		t.Error("a task list holds tasks and nothing else")
	}
	if !all.Holds("VEVENT") || !all.Holds("VJOURNAL") || all.Holds("") {
		t.Error("a calendar of everything holds the three and not nothing")
	}
}
