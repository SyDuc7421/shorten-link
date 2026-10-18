-- Restores the surrogate id; rows get new ids.
ALTER TABLE analytics_daily
  DROP PRIMARY KEY,
  ADD COLUMN id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY FIRST,
  ADD UNIQUE KEY uk_link_stat_date (link_id, stat_date);
