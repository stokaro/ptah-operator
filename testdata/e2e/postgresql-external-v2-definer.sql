CREATE TABLE e2e_widgets (
  id bigint NOT NULL PRIMARY KEY,
  name text NOT NULL
);
CREATE FUNCTION e2e_widget_count() RETURNS bigint
  LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public
  AS $$ SELECT count(*) FROM public.e2e_widgets $$;
