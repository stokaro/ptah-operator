CREATE TABLE customers (
  id bigint NOT NULL PRIMARY KEY,
  email text NOT NULL,
  signed_up_at timestamptz
);

CREATE TABLE orders (
  id bigint NOT NULL PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES customers (id),
  total_cents bigint NOT NULL
);
