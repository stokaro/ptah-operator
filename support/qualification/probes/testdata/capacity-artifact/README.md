# Native capacity artifact reading

This manifest and its five file layers were read from the disposable registry
for the MySQL populated-input run on operator source `1fcdde7e` and Ptah source
`f6e562c5b0986cd29a53a5cc01938827336b780a`.

The initial readback used the generic Ptah file-layer media type. The publisher
actually writes `application/vnd.stokaro.ptah.migration.file.v1`. Keeping the
native manifest makes that mismatch fail in the local test suite. The files
contain only the deterministic capacity fixture and its checksum inventory.
