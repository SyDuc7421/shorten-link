CREATE TABLE IF NOT EXISTS analytics_daily (
    id        BIGINT   NOT NULL AUTO_INCREMENT,
    link_id   BIGINT   NOT NULL,
    stat_date DATE     NOT NULL,
    clicks    INT      NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_link_stat_date (link_id, stat_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS audit_log (
    id            BIGINT       NOT NULL AUTO_INCREMENT,
    user_id       BIGINT       NOT NULL,
    action        VARCHAR(100) NOT NULL,
    resource_type VARCHAR(50)  NOT NULL,
    resource_id   BIGINT,
    metadata      JSON,
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
