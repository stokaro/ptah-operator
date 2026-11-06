CREATE TABLE orders (id bigint PRIMARY KEY, customer_email varchar(200) NOT NULL);
CREATE UNIQUE INDEX orders_one_per_customer ON orders (customer_email);
