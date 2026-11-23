// The lifecycle explorer's content: one scenario per resource family, the
// objects each touches and how they relate. LifecycleExplorer.astro renders it
// twice, as the interactive view and as the text a reader without JavaScript,
// a search index or a printer gets, and scripts/check-lifecycle-explorer.mjs
// holds the page to it. Nothing here is rendered by hand anywhere else.
//
// Every statement comes from a page of this site, and each stage links the
// pages it was taken from. Links are relative to the page the explorer is
// embedded in, reference/reconciliation/, so they carry the version's base
// path and the link checker reads them like any other link on that page.
//
// A family is a scenario, its paths, its objects and their relations. The
// schema family's scenario is the one First schema walks: a PtahSchema named
// `application` in the namespace `application`, read from a ConfigMap, under
// `apply: OnApproval`, with a plan that changes something. The migration
// family's is the one Run versioned migrations walks: a PtahMigration named
// `orders` in the same namespace, read from a ConfigMap of the two migrations
// in examples/configmap-kustomize, created without Kustomize, under the
// default `apply: OnApproval`.
//
// A scenario's stages are its successful path. Each other path names the
// stage it turns after and the one it comes back at, if it does, and `walkOf`
// joins them. A path turns off the successful path, or off another path when
// the stage it names is that path's own, as an acknowledgment turns off a run
// nobody accounted for. Stage, path and object ids are unique across families,
// so a link that names one names its family too. The audit view, the composed
// workflow and the depth levels are later slices of #817.

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

// The schema family. Its objects are a schema's own: the plan holds the SQL,
// in chunks, and an Apply nobody can account for is settled by a fresh
// observation rather than by a person.
const schemaObjects = [
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
const schemaRelations = [
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
const schemaScenario = {
  id: 'schema-change',
  title: 'One schema change',
  summary:
    'A PtahSchema read from a ConfigMap under apply: OnApproval, from kubectl apply to In sync. Choose a path to follow it where it turns: a plan Ptah refuses, a plan replaced before anyone approved it, or an Apply nobody can account for. The commands use the names from First schema.',
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

// The paths through the scenario. The first is the default, and its stages
// are scenario.stages. Every other one turns after the stage `turnsAfter`
// names, walks its own `stages`, and comes back at the stage `rejoins` names,
// or ends on its own when it names none. Both name stages of the path it turns
// off: the successful one, or the path whose own stage `turnsAfter` is. A
// branch stage is written like a successful one, and its id is unique across
// all paths, so a link that names it names its path too.
const schemaPaths = [
  {
    id: 'success',
    label: 'Successful',
    title: 'A successful schema change',
    summary: 'Nothing refused and nothing failing, from kubectl apply to In sync.',
  },
  {
    id: 'refused',
    label: 'Refused plan',
    title: 'Ptah refuses the plan',
    summary:
      'The Plan runs, and Ptah refuses to plan the change under a code. No plan is published and nothing runs. The schema stops Blocked until an input changes, and then plans again.',
    turnsAfter: 'observe',
    rejoins: 'plan',
    stages: [
      {
        id: 'refused-plan',
        title: 'Ptah refuses the plan',
        phase: 'Planning, then Blocked',
        lead: 'executor',
        actors: {
          operator:
            'Takes the database-realm Lease and creates a Plan Job, as on the successful path. It publishes no plan and leaves the schema Blocked. It does not try the Plan again at the failure retry: another Plan of the same schema against the same server, as the same account, gets the same answer.',
          kubernetes: 'Runs the Plan Pod. The Job is collected five minutes after it finishes.',
          executor:
            'Ptah compares the schema with the database, then refuses under a code, from the plan or from the dry run of the saved plan: a view whose security option or definer the schema does not state, an owner the server would refuse, a backslash its sql_mode reads another way, or a declaration the target cannot host. Its sentence, which names the objects and says what to change, goes to the Plan Pod’s log.',
        },
        objects: ['job', 'lease', 'status'],
        expect:
          'Phase Blocked. Ready=False with reason PlanRefused and the message plan_refused: ptah refused to plan the change (<code>), and a PlanRefused Warning Event with the same message. PlanReady and ReconciliationFailed are false, and DatabaseReachable is true.',
        inspect: [
          { command: 'kubectl ptah schema application -n application', says: 'Phase Blocked, and Ready=False with reason PlanRefused and the code.' },
          { command: 'kubectl events -n application --for ptahschema/application', says: 'The PlanRefused Warning, with the same message.' },
        ],
        done: 'The schema rests at Blocked with reason PlanRefused. No plan waits, so there is nothing to approve.',
        limits: 'Blocked is a refusal, not a fault: the answer does not change until somebody changes an input. Status never carries Ptah’s sentence.',
        links: [
          { label: 'A Plan that stops without saying why', href: '../../troubleshoot/condition-reasons/#a-plan-that-stops-without-saying-why' },
          { label: 'Blocked is a refusal, not a fault', href: '#blocked-is-a-refusal-not-a-fault' },
        ],
      },
      {
        id: 'refused-read',
        title: 'Read why it refused',
        phase: 'Blocked',
        lead: 'you',
        actors: {
          you: 'Read the code in the message, then Ptah’s sentence: the line of the Plan Pod’s log that starts with ptah-runner: ptah reported:. Reading the log takes pods/log in the namespace.',
          operator: 'Reads the source again at spec.interval, at once when a ConfigMap source or the spec changes, or on kubectl ptah refresh.',
        },
        objects: ['job', 'status'],
        inspect: [
          { command: 'kubectl -n application get jobs', says: 'The Plan Job, while it is kept.' },
          {
            command: 'kubectl logs -n application job/<plan job> -c ptah',
            says: 'Ptah’s sentence, which names the objects and says what to change. Once the Job is gone, kubectl ptah refresh runs a new Plan, which writes a fresh log.',
          },
        ],
        done: 'You know the code and the objects it names.',
        links: [
          { label: 'What Ptah refused, and what to do', href: '../../troubleshoot/condition-reasons/#a-plan-that-stops-without-saying-why' },
          { label: 'Condition reasons', href: '../../troubleshoot/condition-reasons/' },
        ],
      },
      {
        id: 'refused-fix',
        title: 'Change the input, then read again',
        phase: 'Blocked, then Planning',
        lead: 'you',
        actors: {
          you: 'Do what the refusal asks: change the declaration in the ConfigMap, list the object in spec.policy.exclude, or fix the server, for a role it does not hold, a privilege the login lacks or its sql_mode. Then ask for a read.',
          operator:
            'An edit to the ConfigMap or the spec starts the read at once. A refusal the server decides lifts only on the server, and nothing the operator watches says when, so the read waits for spec.interval or for kubectl ptah refresh. A refresh starts no Apply and approves nothing.',
        },
        objects: ['source', 'schema', 'status'],
        expect: 'A new Plan. When Ptah plans the change, the walk goes on as on the successful path, at Publish the plan.',
        inspect: [
          {
            command: 'kubectl ptah refresh schema/application -n application --wait',
            says: 'Stays until the read it asked for has ended. Exit 8 means that read ended at Blocked again.',
          },
        ],
        done: 'The next Plan publishes a plan.',
        limits: 'The operator rolls nothing back and changes nothing on the server. Without --wait, refresh says only that the request was written, which is not the read.',
        links: [
          { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
          { label: 'Reading again on request', href: '#reading-again-on-request' },
        ],
      },
    ],
  },
  {
    id: 'stale',
    label: 'Stale plan',
    title: 'The plan is replaced before approval',
    summary:
      'A plan waits for an approval, and an input changes before anyone approves it. The plan is retired, a fresh read makes its replacement, and only an approval of the replacement counts.',
    turnsAfter: 'plan',
    rejoins: 'decision',
    stages: [
      {
        id: 'stale-input',
        title: 'An input changes while the plan waits',
        phase: 'AwaitingApproval, then Observing, Planning and AwaitingApproval',
        lead: 'operator',
        actors: {
          you: 'Edit the ConfigMap or the schema’s spec while the plan waits. Another writer changing the database has the same effect.',
          operator:
            'A changed generation, policy, artifact, coordination key, route identity or observed state retires the plan before Apply. The operator clears status.plan, marks any approval of the old plan stale, reads the database again and plans again. The new plan’s PlanPublished entry names the old one as replaced, and a PlanStale Warning Event names both.',
          executor: 'Runs a fresh Observe and Plan, as on the successful path.',
        },
        objects: ['schema', 'source', 'status', 'plan', 'job'],
        expect:
          'On the way, PlanReady=False with reason Stale and Ready=False with reason StalePlan. Then a new plan, phase AwaitingApproval, and ApprovalRequired=True with reason Waiting.',
        inspect: [
          {
            command: 'kubectl events -n application --for ptahschema/application',
            says: 'PlanStale: the old plan will not be applied, the new one replaced it before it ran, and an approval of the old one no longer counts.',
          },
          {
            command: 'kubectl ptah history schema/application -n application',
            says: 'PlanPublished for the new plan, with replaced naming the old one. The Event can be lost; the journal entry is the record.',
          },
        ],
        done: 'A replacement plan waits for an approval.',
        limits: 'The replacement is another plan with its own fingerprint. Nothing about the old plan, its review or its approval carries over.',
        links: [
          { label: 'Where the path turns', href: '#where-the-path-turns' },
          { label: 'Events', href: '../../troubleshoot/events/' },
          { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
        ],
      },
      {
        id: 'stale-approval',
        title: 'An approval of the old plan approves nothing',
        phase: 'AwaitingApproval',
        lead: 'you',
        actors: {
          you: 'Answer kubectl ptah approve for the plan it showed you, and it reads the schema once more before it sends anything. A newer plan replaced that one, so it approves nothing and exits 4. Read the replacement, then approve that.',
          kubernetes: 'Admission rejects an approval of a plan that is already stale, and one that names a plan UID or a fingerprint the current plan does not carry.',
          operator: 'An approval stored before the plan was replaced gets Stale=True with reason PlanNoLongerCurrent and an ApprovalStale Warning Event. It is never consumed.',
        },
        objects: ['approval', 'plan', 'status'],
        expect: 'kubectl ptah schema lists the old approval as Stale, naming a plan that is not the current one.',
        inspect: [
          {
            command: 'kubectl ptah schema application -n application',
            says: 'The approvals that name the schema, with Accepted, Consumed and Stale, and whether each names the current plan.',
          },
          { command: 'kubectl ptah plan application -n application', says: 'The replacement, with its own fingerprint and statements.' },
        ],
        done: 'No approval counts for the replacement yet. The walk goes on at Approve that exact plan, for the replacement.',
        links: [
          { label: 'Exact-plan approvals', href: '../../use/approvals/' },
          { label: 'Condition reasons', href: '../../troubleshoot/condition-reasons/' },
        ],
      },
    ],
  },
  {
    id: 'uncertain',
    label: 'Uncertain Apply',
    title: 'An Apply nobody can account for',
    summary:
      'The Apply ends with no proof of what it did. The operator never runs the plan again: a fresh read-only observation establishes what the database holds, and the schema recovers forward from there.',
    turnsAfter: 'execute',
    stages: [
      {
        id: 'unknown-outcome',
        title: 'The Apply ends without proof',
        phase: 'Applying, then VerifyingConvergence',
        lead: 'operator',
        actors: {
          executor: 'Ptah reports an error, or the run leaves nothing the manager can read: missing output, a timeout, a cancellation or malformed output.',
          kubernetes: 'Keeps the Apply Job and its log for 24 hours after it finishes, the cleanup TTL of an Apply a person approved whose outcome is unknown.',
          operator:
            'Records every Apply error as an unknown outcome, because another Pod of the same Job may have run. It keeps the Lease under the Apply’s holder, records the proof it owes in status.pendingObservation, writes ApplyFinished with outcome Unknown, and permits only a read-only observation. The plan is never replayed, and the approval was spent at dispatch.',
        },
        objects: ['job', 'result', 'status', 'lease', 'approval'],
        expect:
          'Applying=False with reason OutcomeUnknown, ReconciliationFailed=True and Ready=False with reason ApplyOutcomeUnknown, and an ApplyOutcomeUnknown Warning Event. When the run wrote something to read, the condition’s message ends with the command that reads the Pod log.',
        inspect: [
          { command: 'kubectl ptah schema application -n application', says: 'Applying=False with reason OutcomeUnknown, and where the Pod log is.' },
          {
            command: 'kubectl ptah schema application -n application --details',
            says: 'Pending proof: the Apply’s own result, OutcomeUnknown, and the time the database is not read before, while the Apply’s Pod could still run.',
          },
          { command: 'kubectl logs -n application job/<apply job> -c ptah', says: 'One line from the run, with the database’s words, after ptah-runner: ptah reported:.' },
        ],
        done: 'The outcome is recorded as unknown, and a proof is owed.',
        limits:
          'Unknown means nobody established the outcome. It does not mean the SQL failed: the database may hold the change, part of it or none of it. kubectl ptah approve --wait stops here with exit 8.',
        links: [
          { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
          { label: 'A statement the database refused', href: '../../troubleshoot/condition-reasons/#a-statement-the-database-refused' },
          { label: 'What a failed Apply leaves in its Pod log', href: '../../use/operations/#failed-apply-log' },
        ],
      },
      {
        id: 'unknown-proof',
        title: 'Prove what the database holds',
        phase: 'VerifyingConvergence',
        lead: 'executor',
        actors: {
          operator:
            'Runs a fresh Observe and Plan before anything else. If the Job’s creation or identity is uncertain, it first waits for the complete possible mutation deadline, renewing the Lease. The observation is bound to the plan’s target.',
          kubernetes: 'Runs the two read-only Jobs.',
          executor: 'Reads the database the plan named and plans the same scope. A Pod whose URL names another target is refused before it connects.',
        },
        objects: ['job', 'lease', 'status'],
        expect: 'A no-change plan, or a plan with changes still to make.',
        inspect: [
          { command: 'kubectl ptah schema application -n application --details', says: 'Pending proof, until the observation settles it.' },
          { command: 'kubectl -n application get ptahschema application -w', says: 'VerifyingConvergence, then InSync, or AwaitingApproval when changes remain.' },
        ],
        done: 'The observation settles the proof the Apply owed.',
        limits:
          'A proof whose target moved stops at Blocked with reason TargetIdentityChanged and keeps its claim and Lease, until the first target is back or a person settles it with kubectl ptah settle.',
        links: [
          { label: 'Where the path turns', href: '#where-the-path-turns' },
          { label: 'A schema whose target moved', href: '../../use/recovery/#a-schema-whose-target-moved' },
        ],
      },
      {
        id: 'unknown-recover',
        title: 'Recover forward',
        phase: 'InSync, or AwaitingApproval',
        lead: 'you',
        actors: {
          operator:
            'With no change, it records InSync=True with reason ConvergedAfterUnknownOutcome and hands the Lease back. With changes, it publishes a new plan, which under OnApproval waits for a new approval, because the Apply spent the one it carried. It rolls nothing back.',
          you: 'With no change, nothing more is owed. With a new plan, read it and decide again: it is another plan, and the earlier approval does not cover it. If the database needs repair first, repair it deliberately, then ask for a read so the observation produces a new plan.',
        },
        objects: ['status', 'plan', 'approval'],
        expect: 'InSync=True with reason ConvergedAfterUnknownOutcome, or a new plan waiting with ApprovalRequired=True.',
        inspect: [
          { command: 'kubectl ptah plan application -n application', says: 'The new plan, when changes remain.' },
          { command: 'kubectl ptah approve schema/application -n application --wait', says: 'A new approval, bound to the new plan, and a wait until it has run and been proved.' },
          { command: 'kubectl ptah refresh schema/application -n application --wait', says: 'A read after a repair, rather than at the next interval.' },
        ],
        done: 'The schema is in sync, or a new plan is approved and goes through the stages from Claim and dispatch the Apply again.',
        limits:
          'Convergence after an unknown outcome credits no Apply with it. The schema records no status.applied for it, so kubectl ptah plan --applied does not show this plan.',
        links: [
          { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
          { label: 'Condition reasons', href: '../../troubleshoot/condition-reasons/' },
          { label: 'Exact-plan approvals', href: '../../use/approvals/' },
        ],
      },
      {
        id: 'unknown-history',
        title: 'Read what is recorded',
        phase: 'InSync, or AwaitingApproval',
        lead: 'you',
        actors: {
          you: 'Read the journal and the Events. ApplyFinished names the claim, the plan, the approval and the Job, and the statement the Apply stopped at where the database answered. Verified, joined to it by operationID, says what the proof found.',
          operator: 'Keeps the journal in the status writes it already makes.',
        },
        objects: ['status', 'approval', 'plan'],
        expect: 'ApplyFinished with outcome Unknown, then Verified with outcome InSync, or DriftRemains when changes remained.',
        inspect: [
          { command: 'kubectl ptah history schema/application -n application', says: 'The journal, with the approval the Apply spent.' },
          { command: 'kubectl events -n application --for ptahschema/application', says: 'The ApplyOutcomeUnknown Warning, while Kubernetes keeps it, about an hour.' },
        ],
        done: 'You know what the operator recorded, and what it could not.',
        limits:
          'The journal carries no SQL text and no messages, and a converged schema does not say which statements ran. A proof that converged does not reconstruct what the Apply did.',
        links: [
          { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
          { label: 'What a resource kept of its past', href: '../../use/read-a-plan/#history' },
        ],
      },
    ],
  },
];


// What the schema family leaves to the reference, said on the page so a
// reader does not take the absence of a path for a path that cannot turn.
const schemaNotCovered = {
  text: 'Other turns, such as nothing to change, apply: Never or Always, a read that fails and a proof whose target moved, are drawn in the reference below.',
  links: [
    { label: 'Where the path turns', href: '#where-the-path-turns' },
    { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
    { label: 'What differs between a schema and a migration', href: '#schema-and-migration' },
  ],
};

// The migration family. A migration plan names versions and checksums and
// holds no SQL, so it has no chunks and no Plan Job: the manager builds it
// from the history reading, and the statements stay in the source. A run
// nobody can account for holds the resource until a reading or a person
// settles it, which is why the family has an acknowledgment and the schema
// family has none.
const migrationObjects = [
  {
    id: 'migration',
    label: 'PtahMigration',
    kind: 'PtahMigration',
    writer: 'you',
    purpose:
      'The desired state: which database, which source holds the numbered migration files, and the policy a run is held to. You write its spec; only the manager writes its status.',
    lives: 'Your namespace, application.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application',
        says: 'The phase, what holds the migration, its source, what the history said, the plan waiting, the last run and every condition.',
      },
    ],
    links: [
      { label: 'PtahMigration fields', href: '../ptahmigration/' },
      { label: 'A migration, not a plan', href: '../../use/read-a-plan/#migration' },
    ],
  },
  {
    id: 'm-source',
    label: 'Source ConfigMap and snapshot',
    kind: 'ConfigMap',
    writer: 'operator',
    purpose:
      'You write the ConfigMap that holds the migration files. The manager copies the items the migration selects into an immutable snapshot ConfigMap named ptah-src-<hash>, with a manifest of every file’s path, size and SHA-256. The History read and the Apply mount the snapshot, never the source, and the SQL an Apply runs comes from it.',
    lives: 'Your namespace, beside the migration.',
    inspect: [
      { command: 'kubectl ptah migration orders -n application', says: 'Source: ConfigMap orders-migrations, and Snapshot, the copy every operation reads.' },
    ],
    links: [
      { label: 'The migration lifecycle', href: '#the-migration-lifecycle' },
      { label: 'Deliver from a ConfigMap, Git or HTTPS', href: '../../use/sources/#from-a-configmap' },
    ],
  },
  {
    id: 'm-status',
    label: 'Status and conditions',
    kind: 'PtahMigration status',
    writer: 'operator',
    purpose:
      'Where the migration stands: status.phase as a summary, conditions as the stable interface, the last reading of the revision table in status.history, the operation claim in status.activeOperation, the last run in status.lastRun, a run nobody accounted for in status.unresolvedRun and how one was settled in status.resolvedRun, and the lifecycle journal in status.journal.',
    lives: 'The status subresource of the PtahMigration. Admission refuses a status write from anyone but the manager.',
    inspect: [
      { command: 'kubectl ptah migration orders -n application', says: 'The phase, the history read, the last run, an unresolved run and every condition.' },
      {
        command: 'kubectl ptah migration orders -n application --details',
        says: 'Every status field the plugin knows: the claim and its Job, a Lease release still owed, the revision rows and each run whole.',
      },
      {
        command: 'kubectl ptah history migration/orders -n application',
        says: 'The journal, entry by entry, with every approval and run acknowledgment that names the migration.',
      },
    ],
    links: [
      { label: 'Every status field the plugin knows', href: '../../use/read-a-plan/#details' },
      { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
      { label: 'Condition reasons', href: '../../troubleshoot/condition-reasons/' },
    ],
  },
  {
    id: 'm-plan',
    label: 'PtahMigrationPlan',
    kind: 'PtahMigrationPlan',
    writer: 'operator',
    purpose:
      'The sequence an Apply may run: each pending version in order with its description and checksum, the history fingerprint and the version it was computed against, and the source by the content digest of its files. The manager builds it from the history reading, with no Job. It is immutable and holds no SQL. Its fingerprint binds the sequence, the history, the target and the execution binding, so a changed input makes another plan.',
    lives: 'Your namespace, beside the migration. Reading it takes get on ptahmigrationplans.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application',
        says: 'The plan, each version with its description, and beneath it the migration’s ApprovalRequired condition. No SQL: the statements stay in the source.',
      },
      { command: 'kubectl ptah migration orders -n application -o json', says: 'The same view as a document, with the plan’s fingerprint as plan.fingerprint, for a script.' },
    ],
    links: [
      { label: 'Immutable bindings', href: '../plans-and-approvals/#immutable-bindings' },
      { label: 'PtahMigrationPlan fields', href: '../ptahmigrationplan/' },
    ],
  },
  {
    id: 'm-approval',
    label: 'PtahMigrationApproval',
    kind: 'PtahMigrationApproval',
    writer: 'you',
    purpose:
      'A person’s decision about one exact plan. It names the migration by name and UID, the plan by name and UID, and the plan’s fingerprint, all three written out. Admission stamps who approved and when, and refuses an approval the current evidence no longer supports. It authorizes one execution.',
    lives: 'Your namespace. It has no owner, so it stays when the migration is deleted.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application',
        says: 'The approvals that name the migration, whether each names the current plan, and Consumed or Stale.',
      },
      { command: 'kubectl ptah history migration/orders -n application', says: 'Every approval that names the migration, oldest first.' },
    ],
    links: [
      { label: 'Approving a run', href: '../../use/migrations/#approving-a-run' },
      { label: 'Exact-plan approvals', href: '../../use/approvals/' },
    ],
  },
  {
    id: 'm-acknowledgment',
    label: 'PtahMigrationRunAcknowledgment',
    kind: 'PtahMigrationRunAcknowledgment',
    writer: 'you',
    purpose:
      'A person’s statement that one run nobody could account for has been accounted for. It names the migration by name and UID and the run by its operationID, and admission stamps who acknowledged and when. It repairs nothing and approves nothing: it lifts the hold, and the history is read again before anything is planned.',
    lives: 'Your namespace. It has no owner, so it stays when the migration is deleted.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application',
        says: 'Run acknowledgments: each with the operation it names, marked when that is the unresolved run, and Consumed or Stale.',
      },
      { command: 'kubectl ptah history migration/orders -n application', says: 'Every acknowledgment that names the migration, oldest first.' },
    ],
    links: [
      { label: 'Settling it by hand', href: '../../use/operations/#clear-unresolved-run' },
      { label: 'PtahMigrationRunAcknowledgment fields', href: '../ptahmigrationrunacknowledgment/' },
    ],
  },
  {
    id: 'm-job',
    label: 'Operation Job',
    kind: 'Job',
    writer: 'operator',
    purpose:
      'One Job per operation that reads or changes the database, here History and Apply; an OCI artifact adds Resolve and Verify. Its Pod runs ptah-runner, which starts the Ptah executor once an init container has checked the snapshot against the content digest. Only this Pod touches the database: the manager holds no database credential. A History Job is removed 300 seconds after it finishes; an Apply Job gets its cleanup TTL only once its result was read.',
    lives: 'Your namespace, beside the migration.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application --details',
        says: 'Active operation: the claim, the Job it names, and whether that Job is confirmed by UID.',
      },
    ],
    links: [
      { label: 'One operation, end to end', href: '../execution/#one-operation-end-to-end' },
      { label: 'The shape of the system', href: '../architecture/#the-shape-of-the-system' },
    ],
  },
  {
    id: 'm-lease',
    label: 'Database-realm Lease',
    kind: 'Lease',
    writer: 'operator',
    purpose:
      'The lock every resource that can change one database takes turns through, so a schema and a migration of one database serialize. It is keyed by a SHA-256 digest of the engine, the namespace and the coordinationKey. A migration takes it for an Apply and nothing else: reading the history changes nothing. It hands it back once the run is accounted for, and keeps it while a run that reported nothing could still be writing.',
    lives: 'The coordination namespace, which is the release namespace unless the chart names another. No owner reference ties it to the migration.',
    inspect: [
      { command: 'kubectl -n <operator namespace> get leases -o wide', says: 'Each Lease with its holder.' },
      { command: 'kubectl ptah migration orders -n application --details', says: 'Lock release: a Lease the run held that is still to be handed back.' },
    ],
    links: [
      { label: 'Concurrency and coordination', href: '../execution/#concurrency-and-coordination' },
      { label: 'The migration lifecycle', href: '#the-migration-lifecycle' },
    ],
  },
  {
    id: 'm-result',
    label: 'PtahResultRecord',
    kind: 'PtahResultRecord',
    writer: 'operator',
    purpose:
      'A Job’s result, stored before its receipt, for a History read as for an Apply. The receiver in the manager authenticates the operation, Job and Pod, persists the whole result, reads it back and only then acknowledges. A repeated delivery returns the same receipt and runs no SQL again. The receipt proves the result arrived, not that a migration applied or that the database holds every one.',
    lives: 'Your namespace. The record is owned by the migration, and its chunks by the record. A collector deletes it at least an hour after retirement.',
    inspect: [
      {
        command: 'kubectl ptah migration orders -n application',
        says: 'Last run: the outcome the manager took from the result. Ordinary inspection reads no result payload: payloads are confidential.',
      },
    ],
    links: [
      { label: 'Durable result protocol', href: '../execution/#durable-result-protocol' },
      { label: 'PtahResultRecord fields', href: '../ptahresultrecord/' },
    ],
  },
];

const migrationRelations = [
  { from: 'migration', to: 'm-source', type: 'references', text: 'spec.artifact.configMap names the source ConfigMap and the items it selects.' },
  { from: 'migration', to: 'm-source', type: 'owns', text: 'The snapshot ConfigMap ptah-src-<hash> is owned by the migration, and deleted once nothing names it.' },
  { from: 'migration', to: 'm-status', type: 'contains', text: 'The status subresource, written by the manager alone.' },
  { from: 'migration', to: 'm-plan', type: 'owns', text: 'Plans are owned by the migration and collected with it.' },
  { from: 'migration', to: 'm-job', type: 'owns', text: 'Every operation Job is owned by the migration. The plan does not own the Job.' },
  { from: 'migration', to: 'm-result', type: 'owns', text: 'A result record is owned by the migration whose operation produced it.' },
  { from: 'm-status', to: 'm-plan', type: 'references', text: 'status.plan names the current plan, and status.lastRun the plan a run carried out.' },
  {
    from: 'm-status',
    to: 'm-job',
    type: 'references',
    text: 'status.activeOperation names the Job before the Job exists, and status.lastRun names it by name and UID after it ran.',
  },
  { from: 'm-status', to: 'm-acknowledgment', type: 'references', text: 'status.resolvedRun names the acknowledgment that settled a run, and who made it.' },
  {
    from: 'm-plan',
    to: 'm-source',
    type: 'references',
    text: 'spec.source binds the source by kind, the ConfigMap’s UID and the content digest of the selected files. The SQL stays there.',
  },
  { from: 'm-approval', to: 'migration', type: 'references', text: 'spec names the migration by name and UID.' },
  { from: 'm-approval', to: 'm-plan', type: 'references', text: 'spec names the plan by name and UID, and its fingerprint.' },
  { from: 'm-acknowledgment', to: 'migration', type: 'references', text: 'spec names the migration by name and UID.' },
  { from: 'm-acknowledgment', to: 'm-status', type: 'references', text: 'spec.operationID names the run status.unresolvedRun records.' },
  { from: 'm-job', to: 'm-plan', type: 'references', text: 'The Apply Job carries the plan’s approved sequence, which Ptah compares with its own selection.' },
  { from: 'm-job', to: 'm-source', type: 'references', text: 'Each operation mounts the snapshot, never the source ConfigMap, and checks it against the content digest first.' },
  { from: 'migration', to: 'm-lease', type: 'holds', text: 'The Apply claim holds the Lease for the run, and only the run.' },
  {
    from: 'm-job',
    to: 'm-result',
    type: 'delivers',
    text: 'The Pod sends its result over TLS to the receiver, which stores it as PtahResultRecord before it acknowledges.',
  },
  { from: 'm-result', to: 'm-status', type: 'delivers', text: 'A background consumer reads the record, and the manager records the outcome in status.' },
];

const migrationScenario = {
  id: 'migration-run',
  title: 'One migration run',
  summary:
    'A PtahMigration read from a ConfigMap under the default apply: OnApproval, from kubectl apply to a history read that finds nothing left to apply. Choose a path to follow it where it turns: a history with nothing pending, or a run that ends Partial or Unknown and holds the migration until a reading or a person settles it. The commands use the names from Run versioned migrations.',
  stages: [
    {
      id: 'm-apply',
      title: 'Apply the migration',
      phase: 'none yet',
      lead: 'you',
      actors: {
        you: 'Create the Secret that holds the database URL and the ConfigMap that holds the migration files, then kubectl apply the PtahMigration. It names no apply policy, so it takes the default, OnApproval.',
        kubernetes: 'The API server validates the object against its schema and stores it.',
        operator: 'Sees the new resource. A new resource has no phase until its first pass.',
      },
      objects: ['migration', 'm-source'],
      inspect: [{ command: 'kubectl ptah migration orders -n application', says: 'The migration and the policy its spec asks for: Policy: apply OnApproval.' }],
      done: 'The PtahMigration exists in the namespace.',
      links: [
        { label: 'Run one from the command line', href: '../../use/migrations/#stage-by-stage' },
        { label: 'Approving a run', href: '../../use/migrations/#approving-a-run' },
      ],
    },
    {
      id: 'm-source',
      title: 'Pin the source',
      phase: 'Reading',
      lead: 'operator',
      actors: {
        operator:
          'Reads the ConfigMap from the API server and copies the selected files into an immutable snapshot, ptah-src-<hash>, owned by the migration. A ConfigMap source runs no Resolve Job and has no verification policy, so there is no Verify step and the phase goes straight to Reading. An OCI artifact takes a Resolve and a Verify Job here, and a Git or HTTPS source a Resolve Job.',
        kubernetes: 'Stores the snapshot ConfigMap.',
      },
      objects: ['m-source', 'm-status'],
      expect: 'Phase Reading, and Progressing=True with reason SnapshotTaken: the ConfigMap was copied into the snapshot and the history is read next. There is no ArtifactVerified condition.',
      inspect: [{ command: 'kubectl ptah migration orders -n application', says: 'Source: ConfigMap orders-migrations, and the Snapshot every operation reads.' }],
      done: 'The snapshot is recorded, and the history read is next.',
      links: [
        { label: 'A ConfigMap source', href: '#a-configmap-source' },
        { label: 'The migration lifecycle', href: '#the-migration-lifecycle' },
      ],
    },
    {
      id: 'm-history',
      title: 'Read the history',
      phase: 'Reading',
      lead: 'executor',
      actors: {
        operator: 'Writes the History claim to status.activeOperation, then creates a read-only History Job. It takes no Lease: reading the history changes nothing.',
        kubernetes:
          'Runs the Job’s Pod. An init container checks the snapshot against the content digest and copies the files before the container that holds the database URL starts.',
        executor:
          'Reads the database’s own revision table against the files: the current version, what is applied and what is pending. No SQL and no error text is read into the revision rows status keeps.',
      },
      objects: ['m-job', 'm-source', 'm-status', 'm-result'],
      expect:
        'History read with its time, Current version, Applied and Pending: 2. Blocked=False with reason HistoryMatched, because the history continues the files, and Ready=False with reason MigrationsPending. The phase moves to Planning.',
      inspect: [
        { command: 'kubectl ptah migration orders -n application', says: 'History read, Current version, Applied and Pending.' },
        {
          command: 'kubectl ptah migration orders -n application --details',
          says: 'Active operation: the History claim and its Job. Once the reading is stored, the newest revision rows under History read.',
        },
      ],
      done: 'The reading is stored in status.history, and the migration moves to Planning.',
      limits:
        'The reading is the database’s record of what ran, not of what the tables hold. A history that is dirty, modified, out of order or ahead stops the migration Blocked here, and nothing is planned.',
      links: [
        { label: 'What the history says', href: '../../use/migrations/#what-the-history-says' },
        { label: 'The migration lifecycle', href: '#the-migration-lifecycle' },
      ],
    },
    {
      id: 'm-plan',
      title: 'Publish the plan',
      phase: 'Planning, then AwaitingApproval',
      lead: 'operator',
      actors: {
        operator:
          'Builds the plan from the reading it holds, with no Job and no Lease: planning a migration is arithmetic on that reading. It creates a PtahMigrationPlan named ptah-mplan-<hash>, owned by the migration, that lists each pending version with its description and checksum and binds the source by its content digest. Under OnApproval it then waits.',
        kubernetes: 'Stores the plan, which is immutable.',
      },
      objects: ['m-plan', 'm-status', 'migration'],
      expect:
        'Phase AwaitingApproval. Ready=False, Progressing=False and ApprovalRequired=True, each with reason AwaitingApproval. The journal records PlanPublished.',
      inspect: [
        {
          command: 'kubectl ptah wait migration/orders --for approval -n application',
          says: 'Returns once a plan waits for an approval. Exit 7 means it timed out, as when nothing is pending, and exit 8 that the history refused.',
        },
        { command: 'kubectl ptah migration orders -n application', says: 'The plan: each version with its description. No SQL: the statements are in the files.' },
      ],
      done: 'A plan waits for an approval. Not one migration in it has run.',
      links: [
        { label: 'A migration, not a plan', href: '../../use/read-a-plan/#migration' },
        { label: 'Immutable bindings', href: '../plans-and-approvals/#immutable-bindings' },
      ],
    },
    {
      id: 'm-decision',
      title: 'Approve that exact plan',
      phase: 'AwaitingApproval',
      lead: 'you',
      actors: {
        you: 'Run kubectl ptah approve. It shows each version with its checksum and the identities the approval will carry, asks for a decision and creates the approval as you. It shows no SQL, so read the files the source holds. If the plan was replaced, it approves nothing.',
        kubernetes:
          'Admission stamps your username and the time, and refuses an approval the evidence no longer supports: a plan that was replaced or is no longer current, a history or a database that moved, or a changed source or execution component. It refuses a username under system:ptah-operator:, which the operator records for runs no person approved. RBAC decides whether you may approve at all.',
        operator:
          'Wakes on the approval and holds it to the plan it names, by the migration’s UID, the plan’s UID and the fingerprint.',
      },
      objects: ['m-approval', 'm-plan', 'migration'],
      expect: 'The command prints each version with its checksum, then Approved:, with the identity admission stamped. The approval carries no condition until the run is dispatched.',
      inspect: [
        {
          command: 'kubectl ptah approve migration/orders -n application --wait',
          says: 'Creates the approval and stays until the run of that plan has ended and the history was read back.',
        },
        { command: 'kubectl ptah migration orders -n application', says: 'The approvals that name the migration, and whether each names the current plan.' },
      ],
      done: 'An approval of the current plan is stored.',
      limits:
        'An approval authorizes one execution of that plan. Deleting it before the run’s Job exists withdraws it; once the Job exists, deleting it changes nothing about the run.',
      links: [
        { label: 'Approving a run', href: '../../use/migrations/#approving-a-run' },
        { label: 'Exact-plan approvals', href: '../../use/approvals/' },
      ],
    },
    {
      id: 'm-dispatch',
      title: 'Claim and dispatch the run',
      phase: 'Applying',
      lead: 'operator',
      actors: {
        operator:
          'Writes the Apply claim to status.activeOperation and takes the database-realm Lease. It persists the admission snapshot, marks the approval consumed and persists dispatchStarted, then makes its one permitted create of the Apply Job. A migration plan holds no SQL, so nothing is copied for the Pod to mount. After that boundary a missing Apply Job is an unknown outcome and is never created again, and a manager that restarts reads the boundary from status.',
        kubernetes: 'Schedules the Pod. A webhook compares the mutated Pod with the admission snapshot before it is scheduled.',
      },
      objects: ['m-status', 'm-approval', 'm-job', 'm-lease'],
      expect: 'Phase Applying. The approval carries Consumed=True with reason DispatchCommitted, and the journal records ApprovalAccepted with the approver.',
      inspect: [
        { command: 'kubectl ptah migration orders -n application --details', says: 'Active operation: the Apply claim and the Job it names.' },
        { command: 'kubectl -n application get ptahmigration orders -w', says: 'The phase as it moves, from elsewhere.' },
      ],
      done: 'The Apply Job exists and is bound to the claim.',
      links: [
        { label: 'One operation, end to end', href: '../execution/#one-operation-end-to-end' },
        { label: 'Execution guarantees', href: '../guarantees/' },
      ],
    },
    {
      id: 'm-execute',
      title: 'Run the approved sequence',
      phase: 'Applying',
      lead: 'executor',
      actors: {
        executor:
          'The runner refuses to start Ptah unless the Pod’s database URL names the target the plan was computed against. Ptah compares the approved sequence with what it selects under the migration lock, just before anything runs, and any difference refuses the run before it changes anything. Every row the run writes to schema_migrations_log names the approver.',
        kubernetes: 'Runs the Pod to its end.',
        operator: 'Waits. The Job’s exit status is not taken as the result: the verdict is the database’s, read from the revision table.',
      },
      objects: ['m-job', 'm-plan', 'm-source', 'm-lease'],
      expect: 'Phase Applying until the result is read.',
      inspect: [{ command: 'kubectl -n application get ptahmigration orders -w', says: 'Applying, then VerifyingHistory and Reading.' }],
      done: 'The Pod has ended and sent its result.',
      links: [
        { label: 'Approving a run', href: '../../use/migrations/#approving-a-run' },
        { label: 'The approver’s name in the target database', href: '../../use/security/#approver-in-the-database' },
      ],
    },
    {
      id: 'm-deliver',
      title: 'Deliver the result',
      phase: 'Applying, then VerifyingHistory',
      lead: 'operator',
      actors: {
        executor: 'The runner uploads one bounded result over TLS with the Pod’s bound token. A lost receipt makes it deliver again, never run Ptah again.',
        kubernetes: 'Stores the PtahResultRecord.',
        operator:
          'The receiver authenticates the operation, Job and Pod, persists the whole result, reads it back and acknowledges. A background consumer reads it, and the manager records status.lastRun with the outcome, the Job, the claim, the plan and the approval that allowed it. It clears status.plan, gives the Job its cleanup TTL and hands the Lease back. The journal records RunFinished.',
      },
      objects: ['m-result', 'm-job', 'm-status', 'm-lease'],
      expect:
        'Last run: Applied, with the versions it applied, and a MigrationRunFinished Event. Phase VerifyingHistory for one pass, with Progressing=True and reason VerifyingConvergence. RunFinished in the journal has outcome Succeeded.',
      inspect: [
        {
          command: 'kubectl ptah migration orders -n application',
          says: 'Last run: the outcome, the Job by name and UID, the operation, the plan and who authorized it.',
        },
      ],
      done: 'The run is recorded, and the history is read back next.',
      limits: 'The receipt proves the result arrived, and Applied is what the run reported. Neither is the reading that settles the migration.',
      links: [
        { label: 'Durable result protocol', href: '../execution/#durable-result-protocol' },
        { label: 'What the run reports', href: '../../use/migrations/#what-the-run-reports' },
      ],
    },
    {
      id: 'm-verify',
      title: 'Read the history back',
      phase: 'VerifyingHistory, then Reading and InSync',
      lead: 'executor',
      actors: {
        operator:
          'Claims a read of the history on the next pass and creates a History Job, as at Read the history, without the Lease. The reading of the database the run named is the verdict, and the manager writes it on the run as status.lastRun.verification.',
        kubernetes: 'Runs the read-only History Job.',
        executor: 'Reads the revision table again.',
      },
      objects: ['m-job', 'm-status', 'm-result'],
      expect:
        'Phase InSync, Ready=True with reason HistoryMatched, Pending: 0, and Read back: InSync at version 2 on the last run. The journal records Verified with outcome InSync.',
      inspect: [
        {
          command: 'kubectl ptah migration orders -n application',
          says: 'Phase InSync, Ready=True with reason HistoryMatched, Pending: 0 and Last run: Applied.',
        },
      ],
      done: 'kubectl ptah approve --wait ends with reached applied.',
      limits: 'InSync means the revision table records every migration the files carry. It says nothing about the data in the tables.',
      links: [
        { label: 'What the run reports', href: '../../use/migrations/#what-the-run-reports' },
        { label: 'Waiting for the operator', href: '../../use/operations/#wait' },
      ],
    },
    {
      id: 'm-journal',
      title: 'Read what happened',
      phase: 'InSync',
      lead: 'you',
      actors: {
        you: 'Read the journal, the revision rows and the Events. Reading changes nothing, and you can read them at any stage.',
        operator: 'Keeps the journal in the status writes it already makes, and reads the history again at its interval.',
      },
      objects: ['m-status', 'm-approval', 'm-plan'],
      expect:
        'PlanPublished, ApprovalAccepted, RunFinished with outcome Succeeded and Verified with outcome InSync, each with its time, and status.history.revisions a row for each migration.',
      inspect: [
        { command: 'kubectl ptah history migration/orders -n application', says: 'The journal, with the approval you made.' },
        {
          command: 'kubectl ptah migration orders -n application --details',
          says: 'The newest rows of the revision table under History read, with how long each migration ran where the table records it.',
        },
        { command: 'kubectl events -n application --for ptahmigration/orders', says: 'The migration’s Events, which go first.' },
      ],
      done: 'In sync, until the next read.',
      limits:
        'The journal keeps at most 64 entries, none more than 30 days older than the newest. The database keeps its own record in schema_migrations_log, which the operator never deletes.',
      links: [
        { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
        { label: 'What a resource kept of its past', href: '../../use/read-a-plan/#history' },
      ],
    },
  ],
};

const migrationPaths = [
  {
    id: 'm-success',
    label: 'Successful',
    title: 'A successful migration run',
    summary: 'Nothing refused and nothing failing, from kubectl apply to a history read with nothing pending.',
  },
  {
    id: 'm-nothing',
    label: 'Nothing pending',
    title: 'The history has nothing pending',
    summary:
      'The history read finds every migration the files carry already applied. No plan is published, nothing asks for an approval, and no Apply runs.',
    turnsAfter: 'm-history',
    stages: [
      {
        id: 'm-nothing-read',
        title: 'Find nothing pending',
        phase: 'Reading, then InSync',
        lead: 'operator',
        actors: {
          executor: 'Reads the revision table, as on the successful path, and finds every migration the files carry recorded applied.',
          operator: 'Records the reading and publishes no plan. Nothing asks for an approval, no Apply Job is created, and the Lease is never taken.',
        },
        objects: ['m-status', 'm-job'],
        expect: 'Phase InSync, Ready=True with reason HistoryMatched, Pending: 0, and no plan.',
        inspect: [
          { command: 'kubectl ptah migration orders -n application', says: 'Phase InSync, History read with Pending: 0, and no plan.' },
          { command: 'kubectl ptah wait migration/orders --for approval -n application', says: 'Times out with exit 7: no plan is coming.' },
        ],
        done: 'The migration rests InSync with no plan.',
        limits: 'A history with nothing pending is the database’s record of what ran. It does not say the tables hold what those migrations made.',
        links: [
          { label: 'Where the path turns', href: '#where-the-path-turns' },
          { label: 'Run one from the command line', href: '../../use/migrations/#stage-by-stage' },
        ],
      },
      {
        id: 'm-nothing-rest',
        title: 'Rest in sync until the next read',
        phase: 'InSync',
        lead: 'you',
        actors: {
          you: 'Nothing is owed. To run a new migration, add its file to the ConfigMap and its item to the resource. After a change made outside the operator, ask for a read rather than waiting for the interval.',
          operator:
            'Reads the history again at spec.interval, at once when a selected key of the ConfigMap or the spec changes, or on kubectl ptah refresh. A read that finds nothing to do writes no journal entry.',
        },
        objects: ['migration', 'm-source', 'm-status'],
        expect: 'InSync after every read that finds nothing pending. A read that finds a new migration publishes a plan, as at Publish the plan on the successful path.',
        inspect: [
          {
            command: 'kubectl ptah refresh migration/orders -n application --wait',
            says: 'Asks for a read now and stays until it has ended. It starts no Apply and approves nothing.',
          },
          { command: 'kubectl ptah history migration/orders -n application', says: 'The journal, which holds no entry for a read that found nothing to do.' },
        ],
        done: 'In sync, until the next read.',
        links: [
          { label: 'Reading again on request', href: '#reading-again-on-request' },
          { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
        ],
      },
    ],
  },
  {
    id: 'm-unresolved',
    label: 'Partial or Unknown',
    title: 'A run nobody can account for',
    summary:
      'The run ends Partial, having committed part of a migration, or Unknown, with evidence nobody could read. It is never retried. The migration publishes no plan and dispatches no Apply until a history read with nothing pending, or a person’s acknowledgment, settles the run.',
    turnsAfter: 'm-execute',
    stages: [
      {
        id: 'm-unresolved-record',
        title: 'The run ends Partial or Unknown',
        phase: 'Applying, then Blocked',
        lead: 'operator',
        actors: {
          executor:
            'Ptah reports a migration that committed some of its statements and not the rest, as MySQL DDL or transactionMode: none can leave, or the run leaves evidence nobody can read.',
          kubernetes: 'Keeps the Apply Job, where one exists, and its Pod log for 24 hours after the Job finishes.',
          operator:
            'Records status.lastRun with the outcome, and status.unresolvedRun with the operation, the Job, the plan and the database, with a copy in the operator.ptah.run/unresolved-run annotation. It never retries the run and rolls nothing back. After a run that reported, it hands the Lease back; after one that reported nothing, or whose Job went missing, it keeps the Lease until the run’s dispatch and execution deadlines have passed.',
        },
        objects: ['m-status', 'm-job', 'm-result', 'm-lease', 'migration'],
        expect:
          'Phase Blocked. Blocked=True, Ready=False and Progressing=False, each with reason ApplyOutcomeUnknown, and a MigrationRunFinished Warning, or MigrationRunUncertain for a run that reported nothing. The journal records RunFinished with outcome Partial or Unknown.',
        inspect: [
          {
            command: 'kubectl ptah migration orders -n application',
            says: 'Last run, then the Unresolved run block: the operation, the Job, the plan, the database and what to do.',
          },
          { command: 'kubectl events -n application --for ptahmigration/orders', says: 'The Warning, with where the run’s Pod log is and for how long.' },
        ],
        done: 'The run is recorded as unresolved, and nothing more runs.',
        limits:
          'Partial and Unknown are never retried: a file that committed half its statements would run them twice, and no controller can know which half. Unknown does not mean that nothing ran. kubectl ptah approve --wait stops here with exit 8.',
        links: [
          { label: 'What the run reports', href: '../../use/migrations/#what-the-run-reports' },
          { label: 'A migration run nobody accounted for', href: '../../use/operations/#a-migration-run-nobody-accounted-for' },
        ],
      },
      {
        id: 'm-unresolved-investigate',
        title: 'Establish what the run did',
        phase: 'Blocked',
        lead: 'you',
        actors: {
          you: 'Read the record, then the database it names, which is the only one the run can have changed: the Pod log while the Job is kept, and Ptah’s own reading of the revision table, at the Ptah version status.executionBinding.ptahVersion names. A replica or a restored copy finds nothing wrong and would settle a record that was telling the truth. The Lease says whether the run could still be writing.',
          operator:
            'Goes on reading the history at spec.interval, and publishes no plan and dispatches no Apply, the migration that run was applying included. Suspension, an unsupported engine, a realm another resource claims and a newer manager’s stored state each hold it before that read.',
        },
        objects: ['m-status', 'm-job', 'm-lease'],
        inspect: [
          { command: 'kubectl ptah migration orders -n application --details', says: 'Every field of the unresolved run, and of the last run beside it.' },
          {
            command: 'kubectl logs -n application job/<apply job> -c ptah',
            says: 'One line from the run with the database’s words, after ptah-runner: ptah reported:, for 24 hours after the Job finishes.',
          },
          {
            command: 'ptah migrations status --db-url "$DATABASE_URL" --migrations-dir ./migrations',
            says: 'Ptah’s own reading, with ./migrations a local copy of the files: a dirty row names its version, its state and how many statements committed, such as state=failed applied=1/2.',
          },
        ],
        done: 'You know what the run left in the database, and whether work is still pending.',
        limits:
          'The record stops this migration from changing the database. It does not mean the database is idle: other resources that share the realm may be working against it.',
        links: [
          { label: 'A migration run nobody accounted for', href: '../../use/operations/#a-migration-run-nobody-accounted-for' },
          { label: 'A migration that failed at a statement', href: '../../use/recovery/#failed-statement' },
          { label: 'What a failed Apply leaves in its Pod log', href: '../../use/operations/#failed-apply-log' },
        ],
      },
      {
        id: 'm-unresolved-read',
        title: 'A read with nothing pending settles it',
        phase: 'Blocked, then InSync',
        lead: 'operator',
        actors: {
          you: 'Finish or undo what the run left, with Ptah: run what is left, or undo what it committed and publish the sequence without that migration. Then ask for a read rather than waiting for the interval.',
          operator:
            'A history read of the database the record names that finds nothing of the files left to apply removes status.unresolvedRun and its annotation and records status.resolvedRun with resolution HistoryRead. The migration is InSync with no spec edit. A read of another database, or one that finds work still pending, keeps the record.',
          executor: 'Reads the revision table, as at Read the history.',
        },
        objects: ['m-status', 'm-job', 'm-source'],
        expect: 'Phase InSync, Ready=True with reason HistoryMatched, and Settled run: the outcome and the operation, by a history read.',
        inspect: [
          {
            command: 'kubectl ptah refresh migration/orders -n application --wait',
            says: 'Asks for the read and stays until it has ended. Exit 8 means it ended Blocked again: work is still pending, and only an acknowledgment settles that.',
          },
          { command: 'kubectl ptah migration orders -n application', says: 'Settled run, by a history read, once the record is gone.' },
        ],
        done: 'The record is gone and the migration is InSync.',
        limits:
          'The read settles the record and credits no run with the result: it does not say who finished the migration, or how. A settlement by reading writes no journal entry of its own.',
        links: [
          { label: 'How it clears', href: '../../use/operations/#how-it-clears' },
          { label: 'Finish what it started', href: '../../use/recovery/#failed-partial' },
          { label: 'Reading again on request', href: '#reading-again-on-request' },
        ],
      },
    ],
  },
  {
    id: 'm-acknowledged',
    label: 'Acknowledged',
    title: 'A person acknowledges the run',
    summary:
      'The database is accounted for and still has work pending, which no reading can settle, so a person acknowledges that one run. The acknowledgment repairs nothing and approves nothing: the history is read again, and what is still pending is planned and waits for a new approval.',
    turnsAfter: 'm-unresolved-investigate',
    stages: [
      {
        id: 'm-ack-write',
        title: 'Acknowledge that run',
        phase: 'Blocked',
        lead: 'you',
        actors: {
          you: 'Write a PtahMigrationRunAcknowledgment that names the migration by name and UID and the run by its operationID, both printed in the Unresolved run block, and create it as yourself. It takes create on ptahmigrationrunacknowledgments in the namespace, which the chart’s approver ClusterRole grants.',
          kubernetes: 'Admission stamps your authenticated identity and the time, and refuses one written for anybody else, or one that names a run the migration does not record now.',
        },
        objects: ['m-acknowledgment', 'migration', 'm-status'],
        expect: 'The acknowledgment is stored, with your identity in spec.acknowledgedBy.',
        inspect: [
          { command: 'kubectl ptah migration orders -n application', says: 'The migration UID and the operation ID, in the Unresolved run block.' },
          {
            command: 'kubectl create -f acknowledgment.yaml',
            says: 'Creates it from a file holding the YAML in Settling it by hand, with both values filled in. No kubectl ptah command writes an acknowledgment.',
          },
        ],
        done: 'An acknowledgment of that run is stored.',
        limits: 'Do not acknowledge a run to make the migration move again. An acknowledgment says the database was accounted for, not what it holds now.',
        links: [
          { label: 'Settling it by hand', href: '../../use/operations/#clear-unresolved-run' },
          { label: 'Where to stop', href: '../../use/operations/#clear-stop' },
        ],
      },
      {
        id: 'm-ack-settle',
        title: 'The run is settled, and the history read again',
        phase: 'Blocked, then Reading and AwaitingApproval',
        lead: 'operator',
        actors: {
          operator:
            'Takes the oldest acknowledgment that names the recorded run, whether or not the migration is suspended. It removes status.unresolvedRun, records status.resolvedRun with the acknowledgment and who made it, and marks the acknowledgment Consumed. Then it reads the history before anything is planned. Work still pending gets a new plan, which under OnApproval waits for a new approval, because the run spent the one it carried.',
          you: 'Read the new plan and decide again, as at Approve that exact plan. A second acknowledgment of the same run is answered Stale and settles nothing.',
        },
        objects: ['m-status', 'm-acknowledgment', 'm-plan', 'm-approval'],
        expect:
          'Blocked=False with reason RunAcknowledged, a Normal UnresolvedRunAcknowledged Event naming who acknowledged which run, and the acknowledgment Consumed=True. The journal records RunAcknowledged. Then a new plan waits with ApprovalRequired=True, or the migration is InSync.',
        inspect: [
          {
            command: 'kubectl ptah migration orders -n application',
            says: 'Settled run, by the acknowledgment and from you, and the acknowledgment with its conditions.',
          },
          { command: 'kubectl ptah history migration/orders -n application', says: 'RunAcknowledged, joined to the run’s RunFinished by operationID.' },
          { command: 'kubectl ptah wait migration/orders --for approval -n application', says: 'Returns once the new plan waits for an approval.' },
        ],
        done: 'The run is settled. A new plan goes on from Approve that exact plan, or the migration is InSync.',
        limits:
          'An acknowledgment is not an approval of the next plan, and it is not a repair. The history read and every refusal it makes still apply: a dirty revision row holds the migration Blocked with HistoryDirty.',
        links: [
          { label: 'What proves it worked', href: '../../use/operations/#clear-evidence' },
          { label: 'The lifecycle journal', href: '#the-lifecycle-journal' },
        ],
      },
    ],
  },
];

const migrationNotCovered = {
  text: 'Other turns, such as an artifact the verification policy refuses, a history that is dirty, modified, out of order or ahead, a run that failed at a statement, a plan replaced before anyone approved it, apply: Never or Always and a read that fails, are drawn in the reference below.',
  links: [
    { label: 'The migration lifecycle', href: '#the-migration-lifecycle' },
    { label: 'Where the path turns', href: '#where-the-path-turns' },
    { label: 'A migration that failed at a statement', href: '../../use/recovery/#failed-statement' },
  ],
};

// The families a reader chooses between, the default first. Choosing another
// starts it at its default path, at its first stage, with nothing selected.
export const families = [
  {
    id: 'schema',
    label: 'Schema',
    scenario: schemaScenario,
    paths: schemaPaths,
    objects: schemaObjects,
    relations: schemaRelations,
    notCovered: schemaNotCovered,
  },
  {
    id: 'migration',
    label: 'Migrations',
    scenario: migrationScenario,
    paths: migrationPaths,
    objects: migrationObjects,
    relations: migrationRelations,
    notCovered: migrationNotCovered,
  },
];

export const defaultFamily = families[0].id;

// The path another path turns off: the one whose own stage it turns after,
// or the default path when that stage is a successful one. `model` is a
// family's { stages, paths }, so the gate's self-test can pass one of its own.
export function parentOf(path, model) {
  if (!path.turnsAfter) return null;
  const owner = model.paths.find(
    (other) => other !== path && (other.stages ?? []).some((stage) => stage.id === path.turnsAfter),
  );
  return owner ?? model.paths[0];
}

// The stages a path walks, in order: the stages of the path it turns off up to
// the stage it turns after, its own, then that path's stages from the stage it
// rejoins at. A path that turns off itself, directly or through others, walks
// nothing before its own stages, and the gate refuses it.
export function walkOf(path, model, seen = new Set()) {
  if (!path.turnsAfter) return model.stages;
  const parent = parentOf(path, model);
  const before = seen.has(parent.id) ? [] : walkOf(parent, model, new Set([...seen, path.id]));
  const turn = before.findIndex((stage) => stage.id === path.turnsAfter);
  const rejoin = path.rejoins ? before.findIndex((stage) => stage.id === path.rejoins) : before.length;
  return [...before.slice(0, turn + 1), ...(path.stages ?? []), ...before.slice(rejoin < 0 ? before.length : rejoin)];
}
