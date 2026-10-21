package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// Settings implements db.Settings.
func (r *repo) Settings(ctx context.Context) (map[string]string, error) {
	type row struct{ key, value string }
	rows, err := sqlutil.Collect(ctx, r.q, func(rows *sql.Rows) (row, error) {
		var out row
		return out, rows.Scan(&out.key, &out.value)
	}, `SELECT name, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", mapErr(err))
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.key] = r.value
	}
	return out, nil
}

// PutSetting implements db.Settings.
func (r *repo) PutSetting(ctx context.Context, key, value string, at time.Time) error {
	const query = `INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE value = new.value, updated_at = new.updated_at`
	if _, err := r.q.ExecContext(ctx, query, key, value, times.Arg(at)); err != nil {
		return fmt.Errorf("store setting %s: %w", key, mapErr(err))
	}
	return nil
}

// DeleteSetting implements db.Settings.
func (r *repo) DeleteSetting(ctx context.Context, key string) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, key); err != nil {
		return fmt.Errorf("forget setting %s: %w", key, mapErr(err))
	}
	return nil
}
