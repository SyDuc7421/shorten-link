CREATE TABLE IF NOT EXISTS users (
    id           BIGINT       NOT NULL,
    email        VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    plan         VARCHAR(50)  NOT NULL DEFAULT 'free',
    quota_daily  INT          NOT NULL DEFAULT 100,
    created_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
