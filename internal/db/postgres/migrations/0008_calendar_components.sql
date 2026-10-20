-- What a calendar holds (#79), comma-separated: a task list is a calendar of
-- its own to DAVx5, made with MKCALENDAR and a supported-calendar-component-set
-- of VTODO, and a PUT of anything else into it is refused. Every calendar made
-- before this held events alone, which is what the default says -- except the
-- default calendar, which every owner has and which takes everything now.
ALTER TABLE calendars ADD COLUMN components TEXT NOT NULL DEFAULT 'VEVENT';

UPDATE calendars SET components = 'VEVENT,VTODO,VJOURNAL' WHERE name = 'default';
