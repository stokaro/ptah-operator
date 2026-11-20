CREATE TABLE differs_notes (
  id bigint NOT NULL PRIMARY KEY,
  body varchar(64) COLLATE utf8mb4_0900_ai_ci NOT NULL
) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
