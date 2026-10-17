-- Fails if two codes differ only in case (e.g. Sale and sale).
ALTER TABLE links MODIFY short_code VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL;
