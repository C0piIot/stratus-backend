-- What a client paints a calendar with (#399): #RRGGBB or #RRGGBBAA, which is
-- what Apple's calendar-color property carries and what DAVx5 reads, and empty
-- for none chosen. internal/calendar checks the shape before it gets here.
ALTER TABLE calendars ADD COLUMN color VARCHAR(9) NOT NULL DEFAULT '' AFTER description;
