CREATE TABLE items (
  id integer PRIMARY KEY,
  qty integer NOT NULL,
  price integer NOT NULL,
  total integer GENERATED ALWAYS AS (qty * price)
);
