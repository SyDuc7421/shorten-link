-- Drops the admin columns. Every admin role and every disabled flag is lost.
ALTER TABLE audit_log DROP KEY idx_created;
ALTER TABLE links DROP COLUMN disabled_at;
ALTER TABLE users DROP COLUMN disabled_at, DROP COLUMN role;
