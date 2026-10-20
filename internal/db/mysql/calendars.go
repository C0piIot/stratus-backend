package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

const (
	calendarColumns       = sqlutil.CalendarColumns
	calendarObjectColumns = sqlutil.CalendarObjectColumns
)

// CreateCalendar implements db.Calendars.
func (r *repo) CreateCalendar(ctx context.Context, c db.Calendar) (db.Calendar, error) {
	c = c.Normalize()
	// No RETURNING here: the id is the one the insert reports.
	const query = `INSERT INTO calendars (owner_id, name, display_name, description, color, components, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	result, err := r.q.ExecContext(ctx, query, c.OwnerID, c.Name, c.DisplayName, c.Description, c.Color, c.Components,
		times.Arg(c.Created))
	if err != nil {
		return db.Calendar{}, fmt.Errorf("create calendar %q: %w", c.Name, mapErr(err))
	}
	if c.ID, err = result.LastInsertId(); err != nil {
		return db.Calendar{}, fmt.Errorf("create calendar %q: %w", c.Name, err)
	}
	c.SyncToken, c.PrunedToken = 0, 0
	return c, nil
}

// Calendars implements db.Calendars.
func (r *repo) Calendars(ctx context.Context, owner string) ([]db.Calendar, error) {
	const query = `SELECT ` + calendarColumns + ` FROM calendars WHERE owner_id = ? ORDER BY name`

	out, err := sqlutil.Collect(ctx, r.q, scanCalendar, query, owner)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", mapErr(err))
	}
	return out, nil
}

// CalendarByName implements db.Calendars.
func (r *repo) CalendarByName(ctx context.Context, owner, name string) (db.Calendar, error) {
	const query = `SELECT ` + calendarColumns + ` FROM calendars WHERE owner_id = ? AND name = ?`

	c, err := sqlutil.ScanCalendar(r.q.QueryRowContext(ctx, query, owner, name), times)
	if err != nil {
		return db.Calendar{}, fmt.Errorf("get calendar %q: %w", name, mapErr(err))
	}
	return c, nil
}

// UpdateCalendar implements db.Calendars.
func (r *repo) UpdateCalendar(ctx context.Context, c db.Calendar) error {
	const query = `UPDATE calendars SET display_name = ?, description = ?, color = ? WHERE owner_id = ? AND name = ?`

	result, err := r.q.ExecContext(ctx, query, c.DisplayName, c.Description, c.Color, c.OwnerID, c.Name)
	if err != nil {
		return fmt.Errorf("update calendar %q: %w", c.Name, mapErr(err))
	}
	return calendarAffected(result, "update calendar", c.Name)
}

// DeleteCalendar implements db.Calendars. The objects and the tombstones go
// with it by cascade.
func (r *repo) DeleteCalendar(ctx context.Context, owner, name string) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM calendars WHERE owner_id = ? AND name = ?`, owner, name)
	if err != nil {
		return fmt.Errorf("delete calendar %q: %w", name, mapErr(err))
	}
	return calendarAffected(result, "delete calendar", name)
}

// CalendarObjects implements db.Calendars.
func (r *repo) CalendarObjects(ctx context.Context, calendarID int64) ([]db.CalendarObject, error) {
	const query = `SELECT ` + calendarObjectColumns + ` FROM calendar_objects WHERE calendar_id = ? ORDER BY name`

	out, err := sqlutil.Collect(ctx, r.q, scanCalendarObject, query, calendarID)
	if err != nil {
		return nil, fmt.Errorf("list calendar %d: %w", calendarID, mapErr(err))
	}
	return out, nil
}

// CalendarObject implements db.Calendars.
func (r *repo) CalendarObject(ctx context.Context, calendarID int64, name string) (db.CalendarObject, error) {
	const query = `SELECT ` + calendarObjectColumns + ` FROM calendar_objects WHERE calendar_id = ? AND name = ?`

	o, err := sqlutil.ScanCalendarObject(r.q.QueryRowContext(ctx, query, calendarID, name), times)
	if err != nil {
		return db.CalendarObject{}, fmt.Errorf("get calendar object %q: %w", name, mapErr(err))
	}
	return o, nil
}

// PutCalendarObject implements db.Calendars.
func (r *repo) PutCalendarObject(ctx context.Context, o db.CalendarObject) (db.CalendarObject, error) {
	o = o.Normalize()
	token, err := r.nextCalendarToken(ctx, o.CalendarID)
	if err != nil {
		return db.CalendarObject{}, fmt.Errorf("put calendar object %q: %w", o.Name, err)
	}
	o.SyncToken = token

	// Not ON DUPLICATE KEY alone: it fires on any unique key, so a UID taken by
	// another name would overwrite that object rather than conflict with it.
	// The counter above holds the calendar's row, so nothing can take the UID
	// between this read and the write.
	const taken = `SELECT name FROM calendar_objects WHERE calendar_id = ? AND uid_hash = ?`
	var holder string
	err = r.q.QueryRowContext(ctx, taken, o.CalendarID, hash(o.UID)).Scan(&holder)
	switch {
	case err == nil && holder != o.Name:
		return db.CalendarObject{}, fmt.Errorf("put calendar object %q: uid held by %q: %w", o.Name, holder, db.ErrConflict)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return db.CalendarObject{}, fmt.Errorf("put calendar object %q: %w", o.Name, mapErr(err))
	}

	const upsert = `INSERT INTO calendar_objects (calendar_id, name, uid, uid_hash, component, data, etag, sync_token, changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE uid = new.uid, uid_hash = new.uid_hash, component = new.component,
			data = new.data, etag = new.etag, sync_token = new.sync_token, changed_at = new.changed_at`
	if _, err := r.q.ExecContext(ctx, upsert, o.CalendarID, o.Name, o.UID, hash(o.UID), o.Component, o.Data, o.ETag,
		o.SyncToken, times.Arg(o.Changed)); err != nil {
		return db.CalendarObject{}, fmt.Errorf("put calendar object %q: %w", o.Name, mapErr(err))
	}

	const revive = `DELETE FROM calendar_tombstones WHERE calendar_id = ? AND name = ?`
	if _, err := r.q.ExecContext(ctx, revive, o.CalendarID, o.Name); err != nil {
		return db.CalendarObject{}, fmt.Errorf("put calendar object %q: %w", o.Name, mapErr(err))
	}
	return o, nil
}

// DeleteCalendarObject implements db.Calendars.
func (r *repo) DeleteCalendarObject(ctx context.Context, calendarID int64, name string, at time.Time) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM calendar_objects WHERE calendar_id = ? AND name = ?`, calendarID, name)
	if err != nil {
		return fmt.Errorf("delete calendar object %q: %w", name, mapErr(err))
	}
	if err = calendarAffected(result, "delete calendar object", name); err != nil {
		return err
	}

	token, err := r.nextCalendarToken(ctx, calendarID)
	if err != nil {
		return fmt.Errorf("delete calendar object %q: %w", name, err)
	}
	const tombstone = `INSERT INTO calendar_tombstones (calendar_id, name, sync_token, deleted_at) VALUES (?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE sync_token = new.sync_token, deleted_at = new.deleted_at`
	if _, err := r.q.ExecContext(ctx, tombstone, calendarID, name, token, times.Arg(at)); err != nil {
		return fmt.Errorf("delete calendar object %q: %w", name, mapErr(err))
	}
	return nil
}

// CalendarChanges implements db.Calendars.
func (r *repo) CalendarChanges(ctx context.Context, calendarID, since int64) ([]db.CalendarChange, error) {
	const query = `SELECT name, sync_token, 0 FROM calendar_objects WHERE calendar_id = ? AND sync_token > ?
		UNION ALL
		SELECT name, sync_token, 1 FROM calendar_tombstones WHERE calendar_id = ? AND sync_token > ?
		ORDER BY 2`

	out, err := sqlutil.Collect(ctx, r.q, scanCalendarChange, query, calendarID, since, calendarID, since)
	if err != nil {
		return nil, fmt.Errorf("changes to calendar %d: %w", calendarID, mapErr(err))
	}
	return out, nil
}

// PruneCalendarTombstones implements db.Calendars.
func (r *repo) PruneCalendarTombstones(ctx context.Context, before time.Time) (int64, error) {
	const mark = `UPDATE calendars SET pruned_token = (
			SELECT MAX(t.sync_token) FROM calendar_tombstones t
			WHERE t.calendar_id = calendars.id AND t.deleted_at < ?)
		WHERE id IN (SELECT calendar_id FROM calendar_tombstones WHERE deleted_at < ?)`
	if _, err := r.q.ExecContext(ctx, mark, times.Arg(before), times.Arg(before)); err != nil {
		return 0, fmt.Errorf("prune calendar tombstones: %w", mapErr(err))
	}

	n, err := sqlutil.Affected(ctx, r.q, `DELETE FROM calendar_tombstones WHERE deleted_at < ?`, times.Arg(before))
	if err != nil {
		return 0, fmt.Errorf("prune calendar tombstones: %w", mapErr(err))
	}
	return n, nil
}

// nextCalendarToken moves the calendar's counter and returns where it landed,
// or ErrNotFound for a calendar that is not there. Two statements, since there
// is no RETURNING; the UPDATE holds the row until the transaction ends, so the
// read sees this write and no other.
func (r *repo) nextCalendarToken(ctx context.Context, calendarID int64) (int64, error) {
	if _, err := r.q.ExecContext(ctx, `UPDATE calendars SET sync_token = sync_token + 1 WHERE id = ?`, calendarID); err != nil {
		return 0, mapErr(err)
	}
	var token int64
	if err := r.q.QueryRowContext(ctx, `SELECT sync_token FROM calendars WHERE id = ?`, calendarID).Scan(&token); err != nil {
		return 0, mapErr(err)
	}
	return token, nil
}

// calendarAffected is ErrNotFound for a statement that matched nothing.
func calendarAffected(result sql.Result, what, name string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %q: %w", what, name, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %q: %w", what, name, db.ErrNotFound)
	}
	return nil
}

func scanCalendar(rows *sql.Rows) (db.Calendar, error) { return sqlutil.ScanCalendar(rows, times) }

func scanCalendarObject(rows *sql.Rows) (db.CalendarObject, error) {
	return sqlutil.ScanCalendarObject(rows, times)
}

func scanCalendarChange(rows *sql.Rows) (db.CalendarChange, error) {
	return sqlutil.ScanCalendarChange(rows)
}
