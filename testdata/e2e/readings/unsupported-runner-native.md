These readings came from the Linux amd64 Docker images built from the exact
`95e8b1e0` source archive. The isolated fixture's publisher read a private
registry image by digest, appended only the copied-source protocol-2 runner,
and read back the resulting immutable manifest and blobs. The selected source
was the exact amd64 platform manifest of the task image. The fixture accepts
one runtime platform; this trial makes no claim about a multi-platform index.

The normal protocol-1 binary emitted its complete transport frame and refused
missing coordination inputs before child execution. This is a transport
control, not a successful Apply. The published protocol-2 binary refused the
same declared protocol 1 before child execution. Its separate OCI authority
guard exited 2, emitted no main result and returned the recorded diagnostic.
The production result parser reads the saved complete frames in
`TestUnsupportedRunnerFrameReadsTheNativePrivateImage`.

The private registry and its publisher, network and image references were
removed after the trial. The AST fixture's compiled-operation tests separately
require a matching protocol to start the controlled child. These readings
measure image construction, immutable publication, native startup and refusal.
They do not prove the installed manager's recovery, Kubernetes acceptance,
published artifact authenticity or future protocol compatibility.

A second native trial exercised the reviewed publisher directly with the full
OCI index, including its attestation descriptor. The input index, resulting
image digest, base source commit and exact overridden source hashes are retained
in `unsupported-runner-native-index.json`. The publisher selected the native
Linux image and verified its child manifest by digest and size. The resulting
image was pulled and executed; both frames and the guard diagnostic matched
these saved readings byte for byte. Its guard again exited 2 with empty stdout.
The publisher now handles this index itself; the trial did not preselect a
platform. Unit controls separately refuse missing and duplicate platforms,
a changed child digest, and a changed child size. All task resources were removed.
