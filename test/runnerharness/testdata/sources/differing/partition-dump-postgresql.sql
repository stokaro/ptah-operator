--
-- PostgreSQL database dump
--

\restrict NAgNamg3pWTtpNywLJqJrvIevNiLkGKL2w2y3s2DAs1btaOwO80ISoLOP1WORg0

-- Dumped from database version 17.11
-- Dumped by pg_dump version 17.11

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

SET default_tablespace = '';

--
-- Name: differs_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.differs_events (
    id bigint NOT NULL,
    at date NOT NULL,
    note text DEFAULT 'x'::text
)
PARTITION BY RANGE (at);


SET default_table_access_method = heap;

--
-- Name: differs_events_2026; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.differs_events_2026 (
    id bigint NOT NULL,
    at date NOT NULL,
    note text DEFAULT 'x'::text
);


--
-- Name: differs_events_rest; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.differs_events_rest (
    id bigint NOT NULL,
    at date NOT NULL,
    note text DEFAULT 'x'::text
);


--
-- Name: differs_events_2026; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.differs_events ATTACH PARTITION public.differs_events_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');


--
-- Name: differs_events_rest; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.differs_events ATTACH PARTITION public.differs_events_rest DEFAULT;


--
-- Name: differs_events differs_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.differs_events
    ADD CONSTRAINT differs_events_pkey PRIMARY KEY (id, at);


--
-- Name: differs_events_2026 differs_events_2026_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.differs_events_2026
    ADD CONSTRAINT differs_events_2026_pkey PRIMARY KEY (id, at);


--
-- Name: differs_events_rest differs_events_rest_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.differs_events_rest
    ADD CONSTRAINT differs_events_rest_pkey PRIMARY KEY (id, at);


--
-- Name: differs_events_note; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX differs_events_note ON ONLY public.differs_events USING btree (note);


--
-- Name: differs_events_2026_note_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX differs_events_2026_note_idx ON public.differs_events_2026 USING btree (note);


--
-- Name: differs_events_rest_note_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX differs_events_rest_note_idx ON public.differs_events_rest USING btree (note);


--
-- Name: differs_events_2026_note_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.differs_events_note ATTACH PARTITION public.differs_events_2026_note_idx;


--
-- Name: differs_events_2026_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.differs_events_pkey ATTACH PARTITION public.differs_events_2026_pkey;


--
-- Name: differs_events_rest_note_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.differs_events_note ATTACH PARTITION public.differs_events_rest_note_idx;


--
-- Name: differs_events_rest_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.differs_events_pkey ATTACH PARTITION public.differs_events_rest_pkey;


--
-- PostgreSQL database dump complete
--

\unrestrict NAgNamg3pWTtpNywLJqJrvIevNiLkGKL2w2y3s2DAs1btaOwO80ISoLOP1WORg0
