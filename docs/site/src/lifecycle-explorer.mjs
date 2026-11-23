// The lifecycle explorer's content: one scenario, the objects it touches and
// how they relate. LifecycleExplorer.astro renders it twice, as the
// interactive view and as the text a reader without JavaScript, a search index
// or a printer gets, and scripts/check-lifecycle-explorer.mjs holds the page to
// it. Nothing here is rendered by hand anywhere else.
//
// Every statement comes from a page of this site, and each stage links the
// pages it was taken from. Links are relative to the page the explorer is
// embedded in, reference/reconciliation/, so they carry the version's base
// path and the link checker reads them like any other link on that page.
//
// The scenario is the one First schema walks: a PtahSchema named
// `application` in the namespace `application`, read from a ConfigMap, under
// `apply: OnApproval`, with a plan that changes something and nothing that
// fails. Error and recovery branches, the migration family and the audit view
// are later slices of #817.

// The four parties a stage can name, in the order every stage lists them.
export const actors = [
  { id: 'you', label: 'You' },
  { id: 'kubernetes', label: 'Kubernetes' },
  { id: 'operator', label: 'Operator' },
  { id: 'executor', label: 'Ptah executor' },
];

// How two objects can be related. Ownership is an owner reference, which the
// garbage collector follows; a reference is a name or a UID one object carries
// and nothing collects by it. The two are kept apart on purpose.
export const relationTypes = [
  {
    id: 'owns',
    label: 'Owns',
    inverse: 'Owned by',
    text: 'An owner reference. Deleting the owner lets the garbage collector delete it.',
  },
  {
    id: 'references',
    label: 'Names',
    inverse: 'Named by',
    text: 'A name, UID or digest one object carries. Nothing is deleted by it.',
  },
  {
    id: 'contains',
    label: 'Contains',
    inverse: 'Part of',
    text: 'A part of the same object, written with it.',
  },
  {
    id: 'holds',
    label: 'Holds',
    inverse: 'Held by',
    text: 'Coordination: a lock held for a while, not ownership and not an order of work.',
  },
  {
    id: 'delivers',
    label: 'Delivers to',
    inverse: 'Delivered from',
    text: 'The path a result travels from the Job back to the operator.',
  },
];

export const objects = [
  {
    id: 'schema',
    label: 'PtahSchema',
    kind: 'PtahSchema',
    writer: 'you',
    purpose:
      'The desired state: which database, which source holds the schema, and the policy a plan is held to. You write its spec; only the manager writes its status.',
    lives: 'Your namespace, application.',
    inspect: [
      { command: 'kubectl ptah schema application -n application', says: 'The phase, what holds the schema, its source, the plan waiting and every condition.' },
    ],
    links: [
      { label: 'PtahSchema fields', href: '../ptahschema/' },
      { label: 'Where a schema stands', href: '../../use/read-a-plan/#schema' },
    ],
  },
  {
    id: 'source',
    label: 'Source ConfigMap and snapshot',
    kind: 'ConfigMap',
    writer: 'operator',
    purpose:
      'You write the ConfigMap that holds schema.sql. The manager copies the selected keys into an immutable snapshot ConfigMap named ptah-src-<hash>, with a manifest of every file’s path, size and SHA-256. Every later operation mounts the snapshot, never the source.',
    lives: 'Your namespace, beside the schema.',
    inspect: [
      { command: 'kubectl ptah schema application -n application', says: 'Source: ConfigMap with its name, and Snapshot, the copy every operation reads.' },
    ],
    links: [
      { label: 'A ConfigMap source', href: '#a-configmap-source' },
      { label: 'Deliver from a ConfigMap, Git or HTTPS', href: '../../use/sources/#from-a-configmap' },
    ],
  },
  {
    id: 'status',
    label: 'Status and conditions',
    kind: 'PtahSchema status',
    writer: 'operator',
    purpose:
      'Where the schema stands: status.phase as a summary, conditions as the stable interface, the operation claim in status.activeOperation, the proof an Apply still owes in status.pendingObservation, and the lifecycle journal in status.journal.',
    lives: 'The status subresource of the PtahSchema. Admission refuses a status write from anyone but the manager.',
    inspect: [
      { command: 'kubectl ptah schema application -n application', says: 'The phase and every condition, with its reason and message.' },
      { command: 'kubectl ptah schema application -n application --details', says: 'Every status field the plugin knows: the claim, its Job, the proof owed, a Lease release still owed.' },
      { command: 'kubectl ptah history schema/application -n application', says: 'The journal, entry by entry, with every approval that names the schema.' },
    ],
    links: [
      { label: 'Every status field the plugin knows', href: '../../use/read-a-plan/#details' },
      { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
      { label: 'Condition reasons', href: '../../troubleshoot/condition-reasons/' },
    ],
  },
  {
    id: 'plan',
    label: 'PtahSchemaPlan',
    kind: 'PtahSchemaPlan',
    writer: 'operator',
    purpose:
      'The only thing an Apply may run. Immutable and created by the controller. Its fingerprint binds the schema UID, the content digest, the source, the observed and desired state, the policy, the target and the execution binding. A changed input makes another plan rather than changing this one.',
    lives: 'Your namespace, beside the schema.',
    inspect: [
      { command: 'kubectl ptah plan application -n application', says: 'The plan’s identity and each statement under its severity, after the plugin checked every chunk.' },
      { command: 'kubectl ptah plan application --applied -n application', says: 'The stored plan the last confirmed Apply ran.' },
    ],
    links: [
      { label: 'Immutable bindings', href: '../plans-and-approvals/#immutable-bindings' },
      { label: 'Read a plan', href: '../../use/read-a-plan/' },
    ],
  },
  {
    id: 'chunks',
    label: 'PtahSchemaPlanChunk',
    kind: 'PtahSchemaPlanChunk',
    writer: 'operator',
    purpose:
      'The plan’s SQL: at most sixteen immutable chunks of at most 512 KiB, for a plan of at most 8 MiB. The plan status commits the chunk UIDs only after every chunk was read back and verified. Just before an Apply, the operator also writes one immutable ConfigMap per chunk, owned by the plan, for the Apply Pod to mount.',
    lives: 'Your namespace. Reading them takes get on ptahschemaplanchunks.',
    inspect: [
      { command: 'kubectl ptah plan application -n application', says: 'Reads every chunk, checks each against the plan, and prints the joined SQL.' },
    ],
    links: [
      { label: 'How the bytes are stored', href: '../plans-and-approvals/#immutable-bindings' },
      { label: 'Exact-plan approvals', href: '../../use/approvals/' },
    ],
  },
  {
    id: 'approval',
    label: 'PtahSchemaApproval',
    kind: 'PtahSchemaApproval',
    writer: 'you',
    purpose:
      'A person’s decision about one exact plan. It names the schema by name and UID, the plan by name and UID, and the plan’s fingerprint. Admission stamps who approved and when, replacing anything a client wrote there. The approver can be another person than the one who changed the schema.',
    lives: 'Your namespace. It has no owner, so it stays when the schema is deleted.',
    inspect: [
      { command: 'kubectl ptah schema application -n application', says: 'The approvals that name the schema, newest first, with Accepted, Consumed and Stale.' },
      { command: 'kubectl describe ptahschemaapproval <name> -n application', says: 'One approval whole.' },
    ],
    links: [
      { label: 'Exact-plan approvals', href: '../../use/approvals/' },
      { label: 'Plans and approvals', href: '../plans-and-approvals/' },
    ],
  },
  {
    id: 'job',
    label: 'Operation Job',
    kind: 'Job',
    writer: 'operator',
    purpose:
      'One Job per operation: Observe, Plan, Apply, and the Observe and Plan that prove an Apply. Its Pod runs ptah-runner, which starts the Ptah executor. Only this Pod touches the database, and it holds no Kubernetes API credential. A read-only Job is removed 300 seconds after it finishes; an Apply Job gets its cleanup TTL only once its result was read.',
    lives: 'Your namespace, beside the schema.',
    inspect: [
      { command: 'kubectl ptah schema application -n application --details', says: 'Active operation: the claim, the Job it names, and whether that Job is confirmed by UID.' },
    ],
    links: [
      { label: 'One operation, end to end', href: '../execution/#one-operation-end-to-end' },
      { label: 'Architecture', href: '../architecture/#deployment-scope' },
    ],
  },
  {
    id: 'lease',
    label: 'Database-realm Lease',
    kind: 'Lease',
    writer: 'operator',
    purpose:
      'The lock every resource that can change one database takes turns through. It is keyed by a SHA-256 digest of the engine, the namespace and the coordinationKey, never by anything a credential carries. The schema holds it for a Plan, an Apply and the proof that follows, and hands it back after the proof.',
    lives: 'The coordination namespace, which is the release namespace unless the chart names another. No owner reference ties it to the schema.',
    inspect: [
      { command: 'kubectl -n <operator namespace> get leases -o wide', says: 'Each Lease with its holder.' },
      { command: 'kubectl ptah schema application -n application --details', says: 'Lock release: a Lease the operation held that is still to be handed back.' },
    ],
    links: [
      { label: 'Concurrency and coordination', href: '../execution/#concurrency-and-coordination' },
      { label: 'Four identities', href: '../architecture/#four-identities' },
    ],
  },
  {
    id: 'result',
    label: 'PtahResultRecord',
    kind: 'PtahResultRecord',
    writer: 'operator',
    purpose:
      'The Job’s result, stored before its receipt. The receiver in the manager authenticates the operation, Job and Pod, persists the whole result, reads it back and only then acknowledges. A repeated delivery returns the same receipt and runs no SQL again. The receipt proves the result arrived, not that the SQL succeeded or that the database is in sync.',
    lives: 'Your namespace. The record is owned by the schema, and its chunks by the record. A collector deletes it at least an hour after retirement.',
    inspect: [
      { command: 'kubectl ptah schema application -n application --details', says: 'The outcome the manager took from the result. Ordinary inspection reads no result payload: payloads are confidential, and read access belongs only to identities allowed to read operation SQL.' },
    ],
    links: [
      { label: 'Durable result protocol', href: '../execution/#durable-result-protocol' },
      { label: 'PtahResultRecord fields', href: '../ptahresultrecord/' },
    ],
  },
];

// Every relationship once, from the object that carries or exercises it. The
// object view lists each from both ends, under the relation's label at one end
// and its inverse at the other.
export const relations = [
  { from: 'schema', to: 'source', type: 'references', text: 'spec.desired.configMap names the source ConfigMap and the keys it selects.' },
  { from: 'schema', to: 'source', type: 'owns', text: 'The snapshot ConfigMap ptah-src-<hash> is owned by the schema, and deleted once nothing names it.' },
  { from: 'schema', to: 'status', type: 'contains', text: 'The status subresource, written by the manager alone.' },
  { from: 'schema', to: 'plan', type: 'owns', text: 'Plans are owned by the schema and collected with it.' },
  { from: 'plan', to: 'chunks', type: 'owns', text: 'The chunks, and the ConfigMaps an Apply mounts, are owned by the plan.' },
  { from: 'schema', to: 'job', type: 'owns', text: 'Every operation Job is owned by the schema. The plan does not own the Job.' },
  { from: 'schema', to: 'result', type: 'owns', text: 'A result record is owned by the schema whose operation produced it.' },
  { from: 'status', to: 'plan', type: 'references', text: 'status.plan names the current plan, and the approval the controller recorded for it.' },
  { from: 'status', to: 'job', type: 'references', text: 'status.activeOperation names the Job before the Job exists.' },
  { from: 'plan', to: 'chunks', type: 'references', text: 'The plan status binds each chunk by UID, size and digest.' },
  { from: 'approval', to: 'schema', type: 'references', text: 'spec names the schema by name and UID.' },
  { from: 'approval', to: 'plan', type: 'references', text: 'spec names the plan by name and UID, and its fingerprint.' },
  { from: 'job', to: 'chunks', type: 'references', text: 'The Apply Pod mounts the chunk ConfigMaps and checks the whole document against the plan’s content digest.' },
  { from: 'job', to: 'source', type: 'references', text: 'Each operation mounts the snapshot, never the source ConfigMap, and checks it against the content digest first.' },
  { from: 'schema', to: 'lease', type: 'holds', text: 'The schema’s claim holds the Lease for a Plan, an Apply and its proof.' },
  { from: 'job', to: 'result', type: 'delivers', text: 'The Pod sends its result over TLS to the receiver, which stores it as PtahResultRecord before it acknowledges.' },
  { from: 'result', to: 'status', type: 'delivers', text: 'A background consumer reads the record, and the manager records the outcome in status.' },
];

// The stages of the scenario, in order. `actors` says what each party does in
// the stage, and `lead` which of them acts; a party with nothing to do is left
// out rather than written as "nothing". `objects` are the ones the stage
// touches, `inspect` the supported way to read it, `done` how to tell the
// stage is over.
export const scenario = {
  id: 'schema-success',
  title: 'A successful schema change',
  summary:
    'A PtahSchema read from a ConfigMap under apply: OnApproval, from kubectl apply to In sync, with nothing refused and nothing failing. The commands use the names from First schema.',
  stages: [
    {
      id: 'apply',
      title: 'Apply the schema',
      phase: 'none yet',
      lead: 'you',
      actors: {
        you: 'Create the Secret that holds the database URL and the ConfigMap that holds schema.sql, then kubectl apply the PtahSchema with apply: OnApproval.',
        kubernetes: 'The API server validates the object against its schema and stores it.',
        operator: 'Sees the new resource. A new resource has no phase until its first read starts.',
      },
      objects: ['schema', 'source'],
      inspect: [
        { command: 'kubectl ptah schema application -n application', says: 'The schema and the policy its spec asks for.' },
      ],
      done: 'The PtahSchema exists in the namespace.',
      links: [
        { label: 'Create what the schema reads', href: '../../start/first-schema/#create-what-the-schema-reads' },
      ],
    },
    {
      id: 'source',
      title: 'Pin the source',
      phase: 'Observing',
      lead: 'operator',
      actors: {
        operator: 'Reads the ConfigMap from the API server and copies the selected keys into an immutable snapshot, ptah-src-<hash>, owned by the schema. The content digest is the SHA-256 of the snapshot’s manifest.',
        kubernetes: 'Stores the snapshot ConfigMap.',
      },
      objects: ['source', 'status'],
      expect: 'ArtifactResolved=True, reason SnapshotTaken. A ConfigMap source runs no Resolve Job and has no Verify step, so the phase goes straight to Observing.',
      inspect: [
        { command: 'kubectl ptah schema application -n application', says: 'Source: ConfigMap, and the Snapshot every operation reads.' },
      ],
      done: 'ArtifactResolved=True with reason SnapshotTaken.',
      links: [
        { label: 'A ConfigMap source', href: '#a-configmap-source' },
      ],
    },
    {
      id: 'observe',
      title: 'Observe the database',
      phase: 'Observing',
      lead: 'executor',
      actors: {
        operator: 'Writes the operation claim to status.activeOperation, then creates a read-only Observe Job.',
        kubernetes: 'Runs the Job’s Pod, which mounts the snapshot and checks it against the content digest before anything else starts.',
        executor: 'Reads the database and validates a read-only drift report. Status receives its digest, a count, the highest severity and category-level aggregates, never the detail.',
      },
      objects: ['job', 'source', 'status', 'result'],
      expect: 'DatabaseReachable=True, reason Observed, and the drift the observation found.',
      inspect: [
        { command: 'kubectl ptah schema application -n application', says: 'Observed, with its time, and Drift, by category.' },
        { command: 'kubectl ptah schema application -n application --details', says: 'Active operation: the Observe claim and its Job.' },
      ],
      done: 'The drift report is recorded and the schema moves to Planning.',
      links: [
        { label: 'What a read does to the database', href: '../execution/#what-a-read-does-to-the-database' },
        { label: 'Normal status progression', href: '../../use/operations/#normal-status-progression' },
      ],
    },
    {
      id: 'plan',
      title: 'Publish the plan',
      phase: 'Planning, then AwaitingApproval',
      lead: 'executor',
      actors: {
        operator: 'Takes the database-realm Lease and creates a Plan Job. It publishes changed bytes as a PtahSchemaPlan and its chunks, and commits the chunk UIDs only after reading every chunk back. Under OnApproval it then waits.',
        kubernetes: 'Stores the plan and its chunks, both immutable.',
        executor: 'Runs two scoped plans, which must be byte-identical, then dry-runs those exact bytes through the Apply path.',
      },
      objects: ['plan', 'chunks', 'lease', 'job', 'status'],
      expect: 'Phase AwaitingApproval, PlanReady=True with reason Published, and ApprovalRequired=True with reason Waiting. The journal records PlanPublished.',
      inspect: [
        { command: 'kubectl ptah wait schema/application --for approval -n application', says: 'Returns once a plan waits for an approval.' },
        { command: 'kubectl ptah plan application -n application', says: 'Each statement under its severity and reason.' },
      ],
      done: 'A plan waits for an approval. Not one statement in it has run.',
      links: [
        { label: 'Read a plan', href: '../../use/read-a-plan/' },
        { label: 'Immutable bindings', href: '../plans-and-approvals/#immutable-bindings' },
      ],
    },
    {
      id: 'decision',
      title: 'Approve that exact plan',
      phase: 'AwaitingApproval',
      lead: 'you',
      actors: {
        you: 'Read the plan, then run kubectl ptah approve. It shows the plan again with the identities the approval will carry, asks for a decision and creates the approval as you. If a newer plan replaced the one shown, it approves nothing.',
        kubernetes: 'Mutating admission stamps your username, UID and groups, the request UID and the time. Validating admission checks the approval against the current schema and plan. RBAC decides whether you may approve at all.',
        operator: 'Holds the approval to the plan it names and records it in status.plan.approval. The journal records ApprovalAccepted.',
      },
      objects: ['approval', 'plan', 'schema', 'status'],
      expect: 'The command prints Approved:, with the identity admission stamped.',
      inspect: [
        { command: 'kubectl ptah approve schema/application -n application --wait', says: 'Creates the approval and stays until the plan has run and the database was read again.' },
        { command: 'kubectl ptah schema application -n application', says: 'The approvals that name the schema, with their conditions.' },
      ],
      done: 'An approval of the current plan is stored.',
      links: [
        { label: 'Exact-plan approvals', href: '../../use/approvals/' },
        { label: 'It stops, and waits for you', href: '../../start/first-schema/#it-stops-and-waits-for-you' },
      ],
    },
    {
      id: 'dispatch',
      title: 'Claim and dispatch the Apply',
      phase: 'Applying',
      lead: 'operator',
      actors: {
        operator: 'Writes the Apply claim, holds the Lease, persists the admission snapshot, marks the approval consumed and persists dispatchStarted. It writes the chunk ConfigMaps and makes its one permitted create of the Apply Job. After that boundary a missing Apply Job is an unknown outcome and is never created again.',
        kubernetes: 'Schedules the Pod. A webhook compares the mutated Pod with the admission snapshot before it is scheduled.',
      },
      objects: ['status', 'approval', 'job', 'chunks', 'lease'],
      expect: 'Phase Applying. The approval carries Consumed=True with reason DispatchCommitted.',
      inspect: [
        { command: 'kubectl ptah schema application -n application --details', says: 'Active operation: the Apply claim and the Job it names.' },
        { command: 'kubectl -n application get ptahschema application -w', says: 'The phase as it moves, from elsewhere.' },
      ],
      done: 'The Apply Job exists and is bound to the claim.',
      links: [
        { label: 'One operation, end to end', href: '../execution/#one-operation-end-to-end' },
        { label: 'Execution guarantees', href: '../guarantees/' },
      ],
    },
    {
      id: 'execute',
      title: 'Run the plan',
      phase: 'Applying',
      lead: 'executor',
      actors: {
        executor: 'The runner refuses to start unless the Pod’s database URL still hashes to the realm and route identity the plan named, and the claim’s deadlines hold. It checks the document against the plan’s content digest, and Ptah applies it. Only a report of applied that names the approved plan’s digest counts as success.',
        kubernetes: 'Runs the Pod to its end.',
        operator: 'Waits. The Job’s exit is not taken as the result.',
      },
      objects: ['job', 'chunks', 'lease'],
      expect: 'Phase Applying until the result is read.',
      inspect: [
        { command: 'kubectl -n application get ptahschema application -w', says: 'Applying, then VerifyingConvergence.' },
      ],
      done: 'The Pod has ended and sent its result.',
      links: [
        { label: 'Concurrency and coordination', href: '../execution/#concurrency-and-coordination' },
      ],
    },
    {
      id: 'deliver',
      title: 'Deliver the result',
      phase: 'Applying, then VerifyingConvergence',
      lead: 'operator',
      actors: {
        executor: 'The runner uploads one bounded result over TLS with the Pod’s bound token. A lost receipt makes it deliver again, never run Ptah again.',
        kubernetes: 'Stores the PtahResultRecord.',
        operator: 'The receiver authenticates the operation, Job and Pod, persists the whole result, reads it back and acknowledges. A background consumer reads it, the manager records the outcome, gives the Job its cleanup TTL and records the proof it owes in status.pendingObservation. The journal records ApplyFinished.',
      },
      objects: ['result', 'job', 'status'],
      expect: 'Applying=False with reason JobCompleted, which says only that the Apply Job finished.',
      inspect: [
        { command: 'kubectl ptah schema application -n application --details', says: 'Pending proof: the Apply’s own result, ApplySucceeded, apart from the verdict still owed.' },
      ],
      done: 'The outcome is recorded and a proof is owed.',
      limits: 'The receipt proves the result arrived. It does not prove the SQL did what the plan said, and it does not make the schema In sync.',
      links: [
        { label: 'Durable result protocol', href: '../execution/#durable-result-protocol' },
      ],
    },
    {
      id: 'verify',
      title: 'Prove the change',
      phase: 'VerifyingConvergence, then InSync',
      lead: 'executor',
      actors: {
        operator: 'Runs Observe and Plan again under the Apply’s Lease. Only a fresh no-change plan establishes convergence. Then it hands the Lease back.',
        kubernetes: 'Runs the two read-only Jobs.',
        executor: 'Reads the database again, bound to the target the plan named, and plans the same scope.',
      },
      objects: ['job', 'lease', 'status'],
      expect: 'Phase InSync, InSync=True with reason ScopedConverged. The journal records Verified with outcome InSync.',
      inspect: [
        { command: 'kubectl ptah schema application -n application', says: 'InSync=True with reason ScopedConverged.' },
      ],
      done: 'kubectl ptah approve --wait ends with reached applied.',
      limits: 'In sync means the managed scope matches as far as the login can read it. It says nothing about the data in the tables.',
      links: [
        { label: 'The schema lifecycle', href: '#the-schema-lifecycle' },
        { label: 'Waiting for the operator', href: '../../use/operations/#wait' },
      ],
    },
    {
      id: 'history',
      title: 'Read what happened',
      phase: 'InSync',
      lead: 'you',
      actors: {
        you: 'Read the journal, the applied plan and the Events. Reading changes nothing, and you can read them at any stage.',
        operator: 'Keeps the journal in the status writes it already makes, and reads the database again at its interval.',
      },
      objects: ['status', 'approval', 'plan'],
      expect: 'PlanPublished, ApprovalAccepted, ApplyFinished with outcome Succeeded and Verified with outcome InSync, each with its time.',
      inspect: [
        { command: 'kubectl ptah history schema/application -n application', says: 'The journal, with the approval you made.' },
        { command: 'kubectl ptah plan application --applied -n application', says: 'The plan the Apply ran. Without --applied the command exits 3: a converged schema has no current plan.' },
        { command: 'kubectl events -n application --for ptahschema/application', says: 'The schema’s Events, which go first.' },
      ],
      done: 'In sync, until the next read.',
      limits: 'The journal keeps at most 64 entries, none more than 30 days older than the newest. It is the operator’s record, not an audit archive.',
      links: [
        { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
        { label: 'What a resource kept of its past', href: '../../use/read-a-plan/#history' },
      ],
    },
  ],
};

// What this slice leaves to later ones, said on the page so a reader does not
// take the absence of a branch for a path that cannot turn.
export const notCovered = {
  text: 'This walk takes the path with nothing refused and nothing failing. A refusal, a stale plan, an uncertain Apply and their recovery are drawn in the reference below, and versioned migrations have a walk of their own.',
  links: [
    { label: 'Where the path turns', href: '#where-the-path-turns' },
    { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
    { label: 'Run versioned migrations', href: '../../use/migrations/#stage-by-stage' },
  ],
};
