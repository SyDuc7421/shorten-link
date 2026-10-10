-- TIMESTAMP cannot store dates after 2038-01-19, so user-chosen expiries
-- beyond that failed with error 1292. DATETIME has no such limit.
-- Values are stored in UTC (the app's driver uses loc=UTC). Existing rows are
-- converted using the session time zone, which is UTC in the official image.
ALTER TABLE links MODIFY expires_at DATETIME NULL;
