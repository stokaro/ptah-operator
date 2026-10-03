CREATE TABLE customers (
  id BIGINT NOT NULL PRIMARY KEY,
  email TEXT NOT NULL,
  signed_up_at DATETIME(6)
);

CREATE TABLE orders (
  id BIGINT NOT NULL PRIMARY KEY,
  customer_id BIGINT NOT NULL,
  total_cents BIGINT NOT NULL,
  CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES customers (id)
);
