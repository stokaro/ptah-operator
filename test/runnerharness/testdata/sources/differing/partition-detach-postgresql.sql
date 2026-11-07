CREATE TABLE differs_events (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at);
CREATE TABLE differs_e1 (id bigint NOT NULL, at date NOT NULL, note text);
CREATE TABLE differs_other (id bigint NOT NULL, at date NOT NULL, note text) PARTITION BY RANGE (at);
CREATE TABLE differs_loose (id bigint NOT NULL, at date NOT NULL, note text);
