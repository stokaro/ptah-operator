# The shared design

These files are a copy of the design Ptah's documentation and ptah.run carry:
`fonts.css` declares the two self-hosted faces, `global.css` holds the Tailwind
theme and the layout measures, and `ptah.css` maps Starlight's own variables
onto the design's tokens and then restyles each surface from `ptah/`.

A copy rather than a dependency, on purpose. This site has to build from this
repository alone — no checkout of Ptah, nothing fetched from its site — so a
shared package would be the one thing the arrangement is against. What that
costs is the thing to know: a change to the design in `stokaro/ptah` does not
arrive here by itself.

The faces under `src/fonts/` are Instrument Sans and IBM Plex Mono, both under
the SIL Open Font License. Their license texts sit beside them.

`src/components/` carries the three header surfaces the design shapes: the
brand row with the version pill, the text links across to Ptah, and the
light/dark toggle. They came from the same place and were adapted to name this
site rather than that one.
