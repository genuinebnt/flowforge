-- 0001_extensions: extensions every schema relies on (spec section 10).
-- pg_trgm backs the graph search index (10.5, 10.7); citext backs
-- case-insensitive slugs and e-mail addresses (10.2).
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS citext;
