-- Drops the notifications table (every notification is lost) and the
-- expires_at index on links.
ALTER TABLE links DROP KEY idx_expires_at;
DROP TABLE IF EXISTS notifications;
