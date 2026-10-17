-- The table default collation (utf8mb4_0900_ai_ci) made short_code
-- case-insensitive: /Sale and /sale hit the same row, and two base62 codes
-- differing only in case collided on uk_short_code. Compare bytes instead.
-- Existing rows cannot conflict: the old unique key already forbade
-- case-only duplicates.
ALTER TABLE links MODIFY short_code VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL;
