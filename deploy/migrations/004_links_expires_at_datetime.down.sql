-- Fails if any expires_at is after 2038-01-19.
ALTER TABLE links MODIFY expires_at TIMESTAMP NULL;
