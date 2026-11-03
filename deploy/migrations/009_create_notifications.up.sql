-- In-app notifications for link owners. A row stores structured fields (type,
-- link, expiry), not display text. dedupe_key is unique so a redelivered
-- event inserts nothing the second time.
-- The index on links(expires_at) lets the expiry scanner find links that are
-- about to expire without reading the whole table.
CREATE TABLE IF NOT EXISTS notifications (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    user_id    BIGINT      NOT NULL,
    type       VARCHAR(32) NOT NULL,
    link_id    BIGINT      NULL,
    short_code VARCHAR(16) NULL,
    expires_at DATETIME    NULL,
    dedupe_key VARCHAR(64) NOT NULL,
    read_at    DATETIME    NULL,
    created_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_dedupe (dedupe_key),
    KEY idx_user_id (user_id, id),
    KEY idx_user_unread (user_id, read_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE links
  ADD KEY idx_expires_at (expires_at);
