CREATE TABLE parent (id bigint PRIMARY KEY);
CREATE TABLE child (id bigint PRIMARY KEY, parent_id bigint REFERENCES parent (id) DEFERRABLE INITIALLY DEFERRED);
