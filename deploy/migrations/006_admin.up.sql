-- Admin site: users get a role (only 'admin' may call /api/admin/*), and
-- users and links can be disabled by an admin without deleting them.
-- audit_log is listed newest first, hence the index on created_at.
ALTER TABLE users
  ADD COLUMN role        VARCHAR(16) NOT NULL DEFAULT 'user' AFTER plan,
  ADD COLUMN disabled_at DATETIME    NULL;
ALTER TABLE links
  ADD COLUMN disabled_at DATETIME NULL;
ALTER TABLE audit_log
  ADD KEY idx_created (created_at);
