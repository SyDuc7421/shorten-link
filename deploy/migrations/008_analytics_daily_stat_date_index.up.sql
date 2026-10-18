-- The admin stats read analytics_daily by date range (stat_date >= ?). The
-- primary key starts with link_id, so that scanned the whole table. This
-- index covers both stats queries (per-day sums and top links).
ALTER TABLE analytics_daily ADD KEY idx_stat_date (stat_date, link_id, clicks);
