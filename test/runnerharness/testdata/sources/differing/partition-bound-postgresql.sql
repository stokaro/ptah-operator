CREATE TABLE differs_events (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at);
CREATE TABLE differs_e1 PARTITION OF differs_events FOR VALUES FROM ('2026-01-01') TO ('2028-01-01');
CREATE TABLE differs_other (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at);
CREATE TABLE differs_loose (id bigint NOT NULL, at date NOT NULL, note text);
