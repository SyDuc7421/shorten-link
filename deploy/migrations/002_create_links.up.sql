CREATE TABLE IF NOT EXISTS links (
    id           BIGINT        NOT NULL,
    user_id      BIGINT        NOT NULL,
    short_code   VARCHAR(16)   NOT NULL,
    original_url TEXT          NOT NULL,
    clicks       BIGINT        NOT NULL DEFAULT 0,
    expires_at   TIMESTAMP     NULL,
    created_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_short_code (short_code),
    KEY idx_user_id (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
