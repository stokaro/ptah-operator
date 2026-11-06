CREATE TABLE orders (id bigint PRIMARY KEY, customer_id bigint NOT NULL, note text);
CREATE UNIQUE INDEX orders_one_per_customer ON orders (customer_id);
