-- Make (link_id, stat_date) the primary key of analytics_daily. As a secondary
-- unique key, a duplicate-key upsert took an exclusive next-key lock, which
-- also locks the gap before the row; two click workers could then deadlock
-- (one upserting an existing day row, the other inserting a neighbouring new
-- one). A duplicate primary key takes only a record lock. The surrogate id
-- was never used.
ALTER TABLE analytics_daily
  DROP COLUMN id,
  DROP INDEX uk_link_stat_date,
  ADD PRIMARY KEY (link_id, stat_date);
