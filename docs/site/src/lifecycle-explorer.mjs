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
// `apply: OnApproval`, with a plan that changes something. Its stages are the
// successful path. `paths` adds the ones that turn off it: a plan Ptah
// refuses, a plan replaced before anyone approved it, and an Apply nobody can
// account for. A path names the successful stage it turns after and the one it
// comes back at, if it does, and `walkOf` joins them. The migration family and
// the audit view are later slices of #817.

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
// are scenario.stages. Every other one turns after the successful stage
// `turnsAfter` names, walks its own `stages`, and comes back at the successful
// stage `rejoins` names, or ends on its own when it names none. A branch stage
// is written like a successful one, and its id is unique across all paths, so
// a link that names it names its path too.
export const paths = [
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

export const defaultPath = paths[0].id;

// The stages a path walks, in order: the successful ones up to the stage it
// turns after, its own, then the successful ones from the stage it rejoins at.
// `main` is the successful path's stages, so the gate's self-test can pass a
// model of its own.
export function walkOf(path, main = scenario.stages) {
  if (!path.turnsAfter) return main;
  const turn = main.findIndex((stage) => stage.id === path.turnsAfter);
  const rejoin = path.rejoins ? main.findIndex((stage) => stage.id === path.rejoins) : main.length;
  return [...main.slice(0, turn + 1), ...(path.stages ?? []), ...main.slice(rejoin < 0 ? main.length : rejoin)];
}

// What this slice leaves to later ones, said on the page so a reader does not
// take the absence of a path for a path that cannot turn.
export const notCovered = {
  text: 'Other turns, such as nothing to change, apply: Never or Always, a read that fails and a proof whose target moved, are drawn in the reference below, and versioned migrations have a walk of their own.',
  links: [
    { label: 'Where the path turns', href: '#where-the-path-turns' },
    { label: 'Failure recovery', href: '../../use/operations/#failure-recovery' },
    { label: 'Run versioned migrations', href: '../../use/migrations/#stage-by-stage' },
  ],
};
