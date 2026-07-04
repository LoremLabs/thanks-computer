package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/utils/fqdn"
)

type ctxKey string

const (
	CtxKeyVersion ctxKey = "v"
	CtxKeyService ctxKey = "service"
	CtxKeyTeam    ctxKey = "team"
	CtxKeyRepo    ctxKey = "repo"
	CtxKeyProject ctxKey = "project"
	CtxKeySubsys  ctxKey = "subsys"
	CtxKeyIsPull  ctxKey = "isPull"
	CtxKeyRef     ctxKey = "ref"
	CtxKeyRid     ctxKey = "rid"
)

// BuildIdentity is the process build identity, set programmatically by
// app.Run at boot (NOT from flags/env — no `id` tag, so the flag loader
// skips it) and surfaced by the admin /healthz JSON. Kept on Config so it
// reaches server/admin via processor.Unit.Conf without a chassis/cli import
// cycle (files under chassis/cli import chassis/server).
type BuildIdentity struct {
	Version        string
	Commit         string
	Chassis        string
	BuildTimestamp string
	InstallMethod  string
}

// Command Line Flags / Environment variables to configure runtime environment
type Config struct {
	// Build is set by app.Run after Load (no `id` tag ⇒ ignored by the flag
	// loader); see BuildIdentity.
	Build BuildIdentity

	AdminAddr                    string   `id:"admin-addr" default:":8081" desc:"The port to listen on for the admin web server (:8081)"`
	AdminPass                    string   `id:"admin-pass" default:"" desc:"Basic Auth password ()"`
	AdminUser                    string   `id:"admin-user" default:"" desc:"User for basic auth ()"`
	AdminIdleTimeout             int      `id:"admin-idle-timeout" default:"60" desc:"Idle timeout, seconds (60)"`
	AdminReadTimeout             int      `id:"admin-read-timeout" default:"15" desc:"Read timeout, seconds (15)"`
	AdminStaticRoot              string   `id:"admin-root-dir" default:"./chassis/data/admin/static" desc:"What is the root directory for the admin web server? (./chassis/data/admin/static)"`
	AdminWriteTimeout            int      `id:"admin-write-timeout" default:"15" desc:"Write timeout, seconds (15)"`
	AdminCorsOrigins             []string `id:"admin-cors-origins" default:"" desc:"Comma-separated browser Origin allowlist (scheme+host[:port], e.g. https://admin.example.com) for cookie-authed admin mutations. Empty keeps only the built-in dev/self origins. Set this when serving the admin UI at a public hostname so it is full mutate-capable instead of read-only."`
	AuthMode                     string   `id:"auth-mode" default:"both" desc:"Admin API authentication mode: {basic, signed, both} (both)"`
	AuthDevEnrollSecret          string   `id:"auth-dev-enroll-secret" default:"" desc:"When set on a non-prod chassis, enables POST /auth/dev/enroll. NEVER set this in production."`
	CloudOAuthIssuer             string   `id:"cloud-oauth-issuer" default:"" desc:"OIDC issuer URL whose id_tokens this chassis trusts for POST /auth/oauth/enroll; its discovery doc resolves the JWKS (oidc-provider /jwks fallback). Empty (default) disables the endpoint — open-core trusts no external issuer until an operator opts in."`
	CloudOAuthAudience           string   `id:"cloud-oauth-audience" default:"" desc:"When set, the id_token aud must contain this value (the OAuth client_id). Empty skips the aud check."`
	CloudChassisURL              string   `id:"cloud-chassis-url" default:"" desc:"Public admin BASE URL echoed to OAuth-enrolled clients in chassis_url (written to the CLI profile). Empty defaults to the request scheme+host."`
	ClientVersionLatest          string   `id:"client-version-latest" default:"" desc:"Latest txco CLI version this server advertises (no leading v, e.g. 0.2.6), surfaced in the admin /healthz JSON for client self-sync. Empty omits it."`
	ClientVersionMinimum         string   `id:"client-version-minimum" default:"" desc:"Minimum supported txco CLI version (no leading v). Clients older than this are warned (warn-only; never blocked) via the /healthz policy. Empty disables the warning."`
	ClientVersionCritical        bool     `id:"client-version-critical" default:"false" desc:"When true, the advertised client-version update is flagged critical in the /healthz policy so out-of-date CLIs warn more loudly. Still warn-only."`
	SecretMasterKeyPath          string   `id:"secret-master-key" default:"./chassis/data/secrets/txco-master.key" desc:"Path to the host-local master-key file for the per-tenant secret store. Auto-minted on first boot if absent (same convention as the runtime DB). File is 32 bytes with 0600 perms. Set to empty to disable the feature entirely (library / embedder opt-out)."`
	SecretMasterKeyB64           string   `id:"secret-master-key-b64" default:"" desc:"Base64-encoded 32-byte master key for the per-tenant secret store, shared across the WHOLE fleet. When set it takes precedence over the per-node --secret-master-key file, so secrets replicated via fleet-sync decrypt on every node. Put the SAME value in every node's env (e.g. /data/secrets/txco/.env). Empty (default) keeps the per-node auto-minted file (single-node)."`
	DemoMode                     bool     `id:"demo-mode" default:"false" desc:"Register the /v1/demo/* execution-hop endpoints (the txcl learning environment served by 'txco demo'). Default false: a normal chassis / 'txco dev' exposes no demo surface, so the admin UI loads the standard interface. 'txco demo' sets TXCO_DEMO_MODE=true. NEVER enable in production."`
	DebugBreakpoints             bool     `id:"debug-breakpoints" default:"false" desc:"Enable breakpoint debugging: inlet stamps _txc.flag_breakpoint=true on every event and reads ?_txc.break=<scope> from HTTP query strings. NEVER enable in production."`
	DebugPrivate                 bool     `id:"debug-private" default:"false" desc:"Inlet stamps _txc.flag_private=true on every event. When set, the response to the client keeps private (underscore-prefixed) fields like _txc that would otherwise be stripped. Useful for development; leave off in production."`
	TraceMode                    string   `id:"trace-mode" default:"off" desc:"Request tracing: {off, summary, full} (off). Writes per-request artifacts under --trace-dir."`
	TraceStore                   string   `id:"trace-store" default:"file" desc:"Trace sink + reader backend: {noop, file}. file folds per-request artifacts under --trace-dir. trace-mode=off forces noop regardless. (file)"`
	TraceDir                     string   `id:"trace-dir" default:"./data/trace" desc:"Directory for trace artifacts when --trace-mode != off, for the file backend (./data/trace)"`
	TraceAsync                   bool     `id:"trace-async" default:"false" desc:"Buffer trace writes through an async worker so the request path never blocks on disk I/O. Recommended in production when tracing is on."`
	TraceBufferSize              int      `id:"trace-buffer-size" default:"1024" desc:"Size of the async trace event queue. When full, additional events are dropped (request path is never blocked). Applies only with --trace-async (1024)."`
	TraceBodyCapBytes            int      `id:"trace-body-cap-bytes" default:"65536" desc:"Maximum bytes kept per body (request payload, step in/out, final response) when --trace-async is on. Larger bodies are truncated; meta.json records the original size (65536, ~64KB)."`
	TraceStreamLongPollMS        int      `id:"trace-stream-longpoll-ms" default:"30000" desc:"Maximum ms to hold a /traces/stream long-poll request open server-side before returning 202 so the client can re-poll with the same cursor (30000). Auto-clamped to stay under the web-write-timeout. Only meaningful when the trace store registers a live-stream Armable (e.g. the NATS overlay)."`
	TraceStreamRingSize          int      `id:"trace-stream-ring-size" default:"1024" desc:"Per-subscription buffer hint for live trace streams. Backends may clamp; oversubscribed buffers drop the oldest events (best-effort live tail, not durable replay) (1024)."`
	CronPeriod                   int      `id:"cron-period" default:"60" desc:"Seconds between cron ticks, seconds (60)"`
	CronQueue                    string   `id:"cron-queue" default:"local" desc:"Cron dispatch queue backend: {local}. local is an in-process channel + worker pool (single node). (local)"`
	CronMaxInflight              int      `id:"cron-max-inflight" default:"32" desc:"Max concurrent cron dispatches the queue runs at once; bounds the per-tick fan-out (32). Backends that bound concurrency their own way may reuse this value."`
	CronSystemTick               bool     `id:"cron-system-tick" default:"false" desc:"Emit a system-wide cron tick (job=default, no tenant) each period, for scheduled work hooked in _sys/boot or routed by a 'default' cron-job ingress binding. Off by default. The per-tenant _cron fan-out runs independently."`
	ScheduledPeriod              int      `id:"scheduled-period" default:"20" desc:"Seconds between scheduled-event poll passes (the 'scheduled' personality). Each pass reclaims stale claims, claims due rows (schedule_at <= now), and fires them into the tenant's _scheduled/0 stack (20). Worst-case firing latency past schedule_at is one period."`
	ScheduledMaxInflight         int      `id:"scheduled-max-inflight" default:"32" desc:"Max concurrent scheduled-event dispatches per poll pass; bounds the fan-out when many events come due at once (32)."`
	ScheduledStaleAfter          int      `id:"scheduled-stale-after" default:"600" desc:"Seconds a claimed scheduled event may sit before the reclaim sweep resets it to pending so another node retries — crash recovery for a node that died mid-fire (600)."`
	ScheduledRetention           int      `id:"scheduled-retention" default:"604800" desc:"Seconds to keep terminal (done/failed) scheduled-event rows before the poller purges them (604800 = 7d)."`
	DbRuntimeDsn                 string   `id:"db-runtime-dsn" default:"file:$db-root-dir/runtime-$env.db" desc:"DSN for runtime database — ops, stacks, versions, files, tenants (file:./chassis/data/db/runtime-$env.db)"`
	DbAuthDsn                    string   `id:"db-auth-dsn" default:"file:$db-root-dir/auth-$env.db" desc:"DSN for auth database — actors, keys, memberships, invitations, browser sessions. Only opened when the admin personality is active. Default is a local SQLite file (file:./chassis/data/db/auth-$env.db);"`
	ScheduledStore               string   `id:"scheduled-store" default:"sqlite" desc:"Backend for the scheduled_events queue (txco://schedule + the 'scheduled' personality): {sqlite}. sqlite is the bundled SQLite file at --scheduled-db-path. (sqlite)"`
	ScheduledDBPath              string   `id:"scheduled-db-path" default:"./chassis/data/scheduled.db" desc:"Path to the bundled scheduled_events SQLite store (used by --scheduled-store=sqlite). Its parent dir is created on boot. (./chassis/data/scheduled.db)"`
	DbRoot                       string   `id:"db-root-dir" default:"./chassis/data/db" desc:"What is the root directory for any local databases? (./chassis/data/db)"`
	DbSchemaDir                  string   `id:"db-schema-dir" default:"./db/schema/sqlite" desc:"What directory contains the db schema? (./db/schema/sqlite)"`
	DialTimeout                  string   `id:"dial-timeout" default:"100ms" desc:"Default rpc dial timeout (100ms)"`
	DockerBuild                  bool     `id:"docker-build" default:"true" desc:"Attempt to build Dockerfile on push? (true)"`
	DockerRemote                 string   `id:"docker-remote" default:"localhost:5001" desc:"Remote docker host (localhost.5001)"`
	DockerRemoteUser             string   `id:"docker-remote-user" default:"admin" desc:"Remote docker user (admin)"`
	DockerRemotePass             string   `id:"docker-remote-pass" default:"" desc:"Remote docker user ()"`
	DockerTmp                    string   `id:"docker-tmp" default:"./chassis/data/tmp/docker" desc:"Temporary space for building docker (./chassis/data/tmp/docker)"`
	DockerTmpClean               bool     `id:"docker-tmp-clean" default:"true" desc:"Cleanup temporary tar after build? (true)"`
	DockerApiVersion             string   `id:"docker-api-version" default:"1.39" desc:"Default Docker API version (1.39)"`
	Environment                  string   `id:"env" default:"dev" desc:"Runtime Environment: {dev, stage, prod, myenv} (dev)"` //  export TXCOMP_ENV=dev-mm
	EtcdEndpointAddrs            []string `id:"etcd-endpoint-addrs" default:"localhost:2379" desc:"Etcd endpoint addresses Ex: :2379 (:2379)"`
	Fqdn                         string   `id:"fqdn" default:"" desc:"Set our fully qualified domain name"`
	IngressConfigPath            string   `id:"ingress-config" default:"" desc:"Path to ingress YAML mapping (hostname/listener/job) → (tenant, stack). Empty disables ingress routing; events fall through to boot/%/0."`
	RequireHostnameVerification  bool     `id:"require-hostname-verification" default:"false" desc:"When true, the data-plane router filters out unverified tenant_hostnames rows so they don't route. Default permissive (false): unverified rows still route, the chassis logs a WARN once per row. Set true in production to gate routing on proof-of-ownership."`
	StructuredHostSuffix         string   `id:"structured-host-suffix" default:"" desc:"When set (e.g. '.stacks.example.com'), the chassis auto-mints one tenant_hostnames row per activated stack at '<stack>-<rand><suffix>' so a freshly-applied stack is reachable with no manual binding. Empty (default) disables minting — embedders unchanged. txco dev injects '.localhost' for zero-flag local use."`
	StructuredDNSSelf            bool     `id:"structured-dns-self" default:"false" desc:"When true (and the 'admin' personality is enabled + --structured-host-suffix set), the chassis seeds + serves an authoritative WILDCARD zone for the suffix (its own A/MX/SPF + per-host DKIM/DMARC), instead of relying on upstream DNS. Off by default; turn on at cutover, once the suffix's NS is delegated to this chassis (e.g. NS stacks.thanks.computer -> ns1/ns2.thanks.computer). (false)"`
	VerifyAllowPrivateAddresses  bool     `id:"verify-allow-private-addresses" default:"false" desc:"When true, the HTTP-01 hostname verifier accepts hostnames that resolve to private/loopback/link-local IPs. Default false (production-safe, SSRF defense). txco dev flips it true so localhost-style workflows function."`
	IngressMissAction            string   `id:"ingress-miss-action" default:"fallthrough" desc:"What happens when no tenant_hostnames row matches the incoming request: 'fallthrough' (default) dispatches to boot/%/0 so operator-authored boot rules can catch-all; 'reject' returns a clean HTTP 404 without invoking the processor. Set 'reject' in production deployments that route everything via tenant_hostnames."`
	K8sNamespace                 string   `id:"k8s-namespace" default:"default" desc:"Kubernetes Namespace (default)"`
	KubeConfig                   string   `id:"kube-config" default:"$USER/.kube/config" desc:"What path to use for Kubernetes Config? ($USER/.kube/config)"`
	KubeCheckInCluster           bool     `id:"kube-check-incluster" default:"true" desc:"Use to prefer in cluster service account over kube config. Tests if we're running inside a cluster? (true)"`
	KVStore                      string   `id:"kvstore" default:"boltdb" desc:"Backend for the op-writable KV (txco://kv/*): {boltdb, redis}. boltdb is an embedded on-disk store; redis is a shared networked store (native TTL + atomic ops). (boltdb)"`
	KVStoreAddrs                 []string `id:"kvstore-addrs" default:"./chassis/data/kv" desc:"List of KVStore addresses, if appropriate. (./chassis/data/kv)"`
	KVStoreBucket                string   `id:"kvstore-bucket" default:"txco" desc:"KVStore bucket (boltdb only) (txco)"`
	KVStorePassword              string   `id:"kvstore-password" default:"" desc:"Password for the redis KV backend. Empty (default) sends no AUTH — correct for boltdb or an unauthenticated redis on a trusted private network. Set it to match the redis 'requirepass' to authenticate the redis KV. ()"`
	KVMaxValueBytes              int      `id:"kv-max-value-bytes" default:"65536" desc:"Per-value byte cap for the txco://kv/* ops; a larger value errors. 0 = unlimited. Guards the store + envelope against oversized values (65536, 64KiB)."`
	KVMaxTTL                     int      `id:"kv-max-ttl" default:"0" desc:"Ceiling (seconds) on a txco://kv/set or kv/incr TTL; a larger requested ttl is clamped down to it. 0 = unlimited. Per-key TTL is opt-in — the default is a persistent key (no expiry)."`
	Logger                       string   `id:"logger" default:"env" desc:"Set Log display type: {env, production, dev, dev-plain} (env)"`
	LogLevel                     string   `id:"log-level" default:"" desc:"Set Logging Level: {debug, info, warn, error, dpanic, panic, and fatal} (info)"`
	LogOps                       string   `id:"log-ops" default:"disabled" desc:"Log operations to {logger,disabled,dir} (disabled)"`
	LogOpsDir                    string   `id:"log-ops-dir" default:"./chassis/data/logs" desc:"Directory to log operations to (./chassis/data/logs)"`
	OpPayloadMax                 int      `id:"op-payload-max" default:"4194304" desc:"maximum message that operations can send/receive in bytes (4194304)"`
	OpMetricsRegex               string   `id:"op-metrics-regex" default:".*" desc:"Regex must match opname to include in Prometheus Logs. Default matches all. Empty string matches none. (.*)"`
	OpTimeout                    string   `id:"op-timeout" default:"5s" desc:"Default operation timeout (5s)"`
	OpTimeoutMax                 string   `id:"op-timeout-max" default:"10m" desc:"Hard cap on per-op SYNC call timeout. WITH timeout overrides exceeding this are rejected at dispatch. Does NOT cap async ops. (10m)"`
	MaxFuelPerRequest            int      `id:"max-fuel-per-request" default:"100000" desc:"Per-request fuel ceiling. Each accounted action (scope-enter=10, repeat-transition=50, EXEC=25, secret-materialize=100) accrues weighted cost; exhaustion halts the request with a structured txco_fuel_exhausted error. Catches expensive non-loop work (wide fan-out, secret floods). Set to 0 to disable enforcement (metering continues). (100000)"`
	OpScopeTTLMax                int      `id:"op-scope-ttl-max" default:"500" desc:"Per-request stage-hop ceiling. Each Run entry decrements _txc.ttl; exhaustion halts with a structured txcl_scope_ttl_exhausted error. The cheap, loop-shape-specific system guard; catches tight loops and cross-stack ping-pongs faster than fuel can. Set to 0 to disable. (500)"`
	OpRepeatPenaltyMs            int      `id:"op-repeat-penalty-ms" default:"20" desc:"Sleep penalty in milliseconds when a request repeats a (from_stage, to_stage) transition. Throttles CPU consumption inside the TTL window. Set to 0 to disable. (20)"`
	AIChatEnvFallback            bool     `id:"ai-chat-env-fallback" default:"true" desc:"When an ai://chat backend's required secret (e.g., OPENROUTER_KEY) is not found in the per-tenant secret store, fall back to a chassis-wide environment-variable lookup of the same name. Default true for developer convenience ('export OPENROUTER_KEY=...' just works); set false on shared deployments to enforce per-tenant isolation — tenants must provision their own keys. (true)"`
	AIDefaultTimeout             string   `id:"ai-default-timeout" default:"60s" desc:"Default per-op timeout for ai://* EXEC dispatches when WITH timeout is absent. Replaces the chassis-wide op-timeout (default 5s) which is too tight for LLM round-trips. Still capped by op-timeout-max. (60s)"`
	EmbedOllamaBaseURL           string   `id:"embed-ollama-base-url" default:"http://localhost:11434" desc:"Base URL for the ai://embed ollama backend (local, keyless). nomic-embed-text and other local embedding models are reached at <base>/api/embed. Point this at a reachable Ollama on shared deployments, or use the OpenAI-direct backend instead. (http://localhost:11434)"`
	VectorDBPath                 string   `id:"vector-db-path" default:"./chassis/data/vector.db" desc:"Path to the bundled vector store (SQLite + sqlite-vec) backing the txco://vector/* ops. Its parent dir is created on boot. A separate file from the runtime DB so large, independently-changing vectors never bloat the config-reload dump. (./chassis/data/vector.db)"`
	VectorStore                  string   `id:"vector-store" default:"sqlite" desc:"Backend for the txco://vector/* ops: {sqlite}. sqlite is the bundled SQLite + sqlite-vec file at --vector-db-path. (sqlite)"`
	DevAutoVerifyLocalHostnames  bool     `id:"dev-auto-verify-local-hostnames" default:"true" desc:"When true, hostname claims for dev-local patterns (localhost, *.localhost, *.local, *.local.thanks.computer) are auto-stamped with verified_at on creation, skipping the DNS-TXT round-trip. Removes a UX speed bump from the every-new-feature smoke loop on a developer machine. Set false on shared deployments so all hostnames go through proof-of-ownership. (true)"`
	AsyncRuntimeDefault          string   `id:"async-runtime-default" default:"10m" desc:"Default runtime budget for an async op (WITH mode=async) when WITH timeout is omitted; seeds the continuation's expiry. Not capped by op-timeout-max. (10m)"`
	AsyncAckTimeout              string   `id:"async-ack-timeout" default:"5s" desc:"How long to wait for an async worker's 202 handoff ack (not the worker's runtime). (5s)"`
	ContinueAfterDefault         string   `id:"continue-after-default" default:"5s" desc:"Default deadline before a WITH mode=continuable op promotes from sync to continuation. Overridable per-op via WITH continue_after. Must be > 0 and < the op's effective timeout. (5s)"`
	ContinuableTimeoutDefault    string   `id:"continuable-timeout-default" default:"10m" desc:"Default runtime budget for a WITH mode=continuable op when WITH timeout is omitted. Bounds the upstream work both pre- and post-promotion. Not capped by op-timeout-max. (10m)"`
	DeferredJoinSlack            string   `id:"deferred-join-slack" default:"60s" desc:"Flat pad added to a deferred-join op's runtime budget when computing the run's reap deadline, covering downstream synchronous scopes. (60s)"`
	ComputeMaxMemoryMB           int      `id:"compute-max-memory-mb" default:"32" desc:"Per-invocation memory cap for a sandboxed compute (op://) in MB. (32)"`
	ComputeMaxWall               string   `id:"compute-max-wall" default:"250ms" desc:"Per-invocation wall-clock cap for a sandboxed compute (op://); the guest is killed if it exceeds this. (250ms)"`
	Personalities                string   `id:"personalities" default:"cron,tcp,web,admin" desc:"Head types to start. Comma delimited. {cron,tcp,web,admin,lmtp,sweep,dns,mailmap} (cron,tcp,web,admin)"`
	Repl                         bool     `id:"repl" default:"false" desc:"Run REPL mode"`
	PromNamespace                string   `id:"prom-namespace" default:"txco" desc:"Set the Prometheus namespace (txco)"`
	PromPeriod                   int      `id:"prom-period" default:"5" desc:"Set the Prometheus reporting period (5)"`
	PushActionTimeout            int      `id:"push-action-timeout" default:"300" desc:"Timeout for push actions, in seconds (300)"`
	RegistryFixed                []string `id:"registry-fixed" default:"" desc:"Set to use a fixed operation name registry. Prefix:Address:Port ex: *:localhost:5858 ()"`
	RepoCloneSource              string   `id:"repoclone" default:"file://./chassis/data/generator/" desc:"What is the Git URL to use for new services (file://./chassis/data/generator/)"`
	RepoStore                    string   `id:"repostore" default:"file" desc:"Which Repo Store to use? {memory, file} (file)"`
	RepoStoreFileDir             string   `id:"repostore-file-dir" default:"./chassis/data/repo" desc:"What is the root directory for the file repo store? (./chassis/data/repo)"`
	ContinuationStore            string   `id:"continuation-store" default:"file" desc:"Continuation/run store backend: {file} (file). s3 is a reserved enterprise seam."`
	ContinuationStoreFileDir     string   `id:"continuation-store-file-dir" default:"./chassis/data/continuations" desc:"Root directory for the file continuation store (./chassis/data/continuations)"`
	ContinuationStoreS3Bucket    string   `id:"continuation-store-s3-bucket" default:"" desc:"Reserved: S3-compatible continuation store bucket (enterprise seam; unused in open core)"`
	ContinuationStoreS3Prefix    string   `id:"continuation-store-s3-prefix" default:"" desc:"Reserved: S3-compatible continuation store key prefix (enterprise seam; unused in open core)"`
	ContinuationCallbackBaseURL  string   `id:"continuation-callback-base-url" default:"" desc:"Base URL workers POST continuation results back to. Empty derives from --fqdn / web addr."`
	ContinuationSweepPeriod      int      `id:"continuation-sweep-period" default:"900" desc:"Seconds between continuation store sweeps; 0 disables the sweeper (900)"`
	ContinuationRetention        int      `id:"continuation-retention" default:"604800" desc:"Seconds after a run's expiry before its docs are purged (604800 = 7d)"`
	ContinuationStaleResumeAfter int      `id:"continuation-stale-resume-after" default:"600" desc:"Seconds a resume-claim may sit before the run is failed as resumer-stale (600)"`
	ArtifactStore                string   `id:"artifact-store" default:"file" desc:"Snapshot/event artifact store backend: {file} (file)"`
	ArtifactStoreFileDir         string   `id:"artifact-store-file-dir" default:"./chassis/data/artifacts" desc:"Root directory for the file artifact store (./chassis/data/artifacts)"`
	FileCASStore                 string   `id:"filecas-store" default:"file" desc:"Tenant FILES/ content-addressed store backend: {file} (file). s3 is the fleet overlay seam."`
	FileCASStoreFileDir          string   `id:"filecas-store-file-dir" default:"./chassis/data/filecas" desc:"Root directory for the file content-addressed store (./chassis/data/filecas)"`
	FileCASStoreS3Bucket         string   `id:"filecas-store-s3-bucket" default:"" desc:"Reserved: S3-compatible filecas bucket (fleet overlay; unused in open core)"`
	FileCASStoreS3Prefix         string   `id:"filecas-store-s3-prefix" default:"" desc:"Reserved: S3-compatible filecas key prefix (fleet overlay; unused in open core)"`
	FileCASCacheBytes            int      `id:"filecas-cache-bytes" default:"67108864" desc:"In-memory LRU budget (bytes) fronting filecas Get (64MiB). 0 disables the cache."`
	FileCASMaxFileBytes          int      `id:"filecas-max-file-bytes" default:"10485760" desc:"Max size of a single FILES/ asset served from the CAS (10MiB); larger is indexed but 404s on serve. Also the per-entry LRU guard."`
	DatasetCacheDir              string   `id:"dataset-cache-dir" default:"./chassis/data/datasets" desc:"Node-local materialise cache for DATASETS/ artifacts (one <hash>.sqlite per content hash) fronting the filecas backend. Fleet nodes point this at persistent disk (e.g. /data/datasets)."`
	DatasetCacheBytes            int      `id:"dataset-cache-bytes" default:"4294967296" desc:"Disk budget (bytes) for the dataset materialise cache; LRU eviction closes the read handle and removes the cached file (4GiB). 0 = unbounded."`
	DatasetMaxFileBytes          int      `id:"dataset-max-file-bytes" default:"4294967296" desc:"Max size of a single DATASETS/ artifact accepted by the blob upload endpoint and enforced again at activation (4GiB)."`
	DatasetMaxRows               int      `id:"dataset-max-rows" default:"200" desc:"Hard cap on rows a txco://dataset query returns; a query's manifest max_rows and the rule's WITH limit clamp under it (200)."`
	SnapshotBootstrapRef         string   `id:"snapshot-bootstrap-ref" default:"" desc:"If set AND the runtime DB is fresh, fetch this artifact ref and bootstrap-restore it before serving. Empty (default) = no bootstrap."`
	FeedSource                   string   `id:"feed-source" default:"nop" desc:"Control-event feed source: {nop, file}. nop (default) disables the applier; single-node unchanged."`
	FeedSourceFileDir            string   `id:"feed-source-file-dir" default:"./chassis/data/feed" desc:"Root directory for the file feed source (./chassis/data/feed)"`
	FeedPollPeriod               int      `id:"feed-poll-period" default:"15" desc:"Seconds between control-event feed polls; applies when feed-source != nop (15)"`
	FeedSink                     string   `id:"feed-sink" default:"nop" desc:"Control-event feed sink (producer): {nop, file}. nop (default) means admin mutations stay local; no events emitted."`
	FeedSinkBatchSize            int      `id:"feed-sink-batch-size" default:"64" desc:"Max outbox rows drained per pump tick when feed-sink != nop (64)"`
	RoomRelay                    string   `id:"room-relay" default:"" desc:"Cross-node room-message relay: empty (default) = in-process only (single node). ()"`
	EgressPolicy                 string   `id:"egress-policy" default:"open" desc:"Outbound op dial policy: {open, private}. open (default) allows any address; private blocks loopback/private/link-local/CGNAT/cloud-metadata and any egress-deny-cidrs."`
	EgressDenyCIDRs              []string `id:"egress-deny-cidrs" default:"" desc:"Extra CIDRs the 'private' egress policy also blocks (comma-separated); for deployment-specific internal ranges. ()"`
	EgressAllowCIDRs             []string `id:"egress-allow-cidrs" default:"" desc:"CIDRs the 'private' egress policy allows even if otherwise blocked (comma-separated); explicit escape hatch for a trusted internal op endpoint. ()"`
	SystemOpstacksDir            string   `id:"system-opstacks-dir" default:"" desc:"Optional workspace dir containing an OPS/ tree whose _-prefixed stacks (OPS/_sys/...) overlay the embedded system default. Empty uses the embedded default only (txco serve). txco dev points this at the workspace."`
	SystemOpstacksWatch          bool     `id:"system-opstacks-watch" default:"false" desc:"Watch the system-opstacks dir's OPS/ tree and hot-recompile on change. Off for serve (static after boot); txco dev enables it."`
	ReadFileMaxBytes             int      `id:"read-file-max-bytes" default:"1048576" desc:"Per-file byte cap for the txco://read-file op. Files larger than this are truncated (entry marked truncated) unless the op runs with strict=true, which errors instead. Guards the envelope against oversized inlined content (1048576, 1MiB)."`
	ServerId                     string   `id:"sid" default:"" desc:"Set the Server Id ()"`
	LMTPListenAddrs              []string `id:"lmtp-listen-addrs" default:":2424" desc:"LMTP listen addresses. Comma list of 'unix:/path' or ':port'. Default :2424 (mirrors the chassis convention of high-port defaults; the well-known LMTP port is 24 but that needs root). Set to empty to explicitly disable the head even when 'lmtp' is in --personalities. (:2424)"`
	LMTPMaxMsgBytes              int      `id:"lmtp-max-msg-bytes" default:"26214400" desc:"Max accepted DATA message size in bytes. Postfix rejects with 552 on overflow. (26214400 ~= 25 MiB)"`
	LMTPMaxRecipients            int      `id:"lmtp-max-recipients" default:"50" desc:"Max RCPT TO addresses per LMTP transaction. (50)"`
	LMTPReadTimeout              string   `id:"lmtp-read-timeout" default:"30s" desc:"Per-command read timeout for the LMTP listener. (30s)"`
	LMTPDataTimeout              string   `id:"lmtp-data-timeout" default:"60s" desc:"DATA phase read timeout for the LMTP listener. (60s)"`
	LMTPRespTimeout              string   `id:"lmtp-resp-timeout" default:"30s" desc:"Pipeline response timeout (envelope dispatch → rule verdict) for an LMTP delivery. (30s)"`
	LMTPHostname                 string   `id:"lmtp-hostname" default:"" desc:"Greeting hostname for the LMTP server. Empty (default) uses os.Hostname(). ()"`
	LMTPDefaultHosts             []string `id:"lmtp-default-hosts" default:"" desc:"Comma list of hosts the chassis answers Strategy A on (tenant.stack[+mod]@<host> parses to <tenant>/<stack>). Empty (default) disables Strategy A. Multiple hosts allowed for operators running several MX-receiving names. ()"`
	MailMapListenAddrs           []string `id:"mailmap-listen-addrs" default:"" desc:"Listen addresses for the mailmap head: a Postfix tcp_table(5) responder that answers the edge MTA's relay_domains lookup ('is <domain> an accepted mail domain?') against tenant_hostnames. Comma list of ':port'/'host:port'/'unix:/path'. Empty (default) disables the head even when 'mailmap' is in --personalities. Bind only where the co-located Postfix reaches it (e.g. the compose network); never a public interface — the responder is unauthenticated. ()"`
	MailMapReadTimeout           string   `id:"mailmap-read-timeout" default:"5s" desc:"Per-request read timeout for the mailmap tcp_table responder. (5s)"`
	MailRelayAddr                string   `id:"mail-relay-addr" default:"" desc:"SMTP submission address the txco://sendmail op hands outbound mail to (host:port), e.g. the edge txco-mail Postfix on the private net. Empty (default) disables sending — the op returns a clear 'no relay configured' error. ()"`
	MailRelayTLS                 string   `id:"mail-relay-tls" default:"none" desc:"TLS mode dialing the mail relay: {none, starttls}. Default 'none' for a trusted private-net relay (same posture as the LMTP inlet); 'starttls' opportunistically upgrades. (none)"`
	MailDialTimeoutMS            int      `id:"mail-dial-timeout-ms" default:"5000" desc:"Dial+submit timeout for the mail relay, milliseconds. A down relay fails the send fast rather than hanging the request. (5000)"`
	MailMaxRecipients            int      `id:"mail-max-recipients" default:"50" desc:"Max recipients per txco://sendmail call (one personalized message + relay submit each, synchronously). Over the cap the op errors rather than truncating; bulk/async is a later story. (50)"`
	MailRateLimits               string   `id:"mail-rate-limits" default:"" desc:"Per-tenant outbound send caps as comma-separated <count>/<duration> rules, e.g. \"100/2m,200/4h\". A send is allowed only if EVERY rule is under its cap; over-limit recipients are skipped (reason rate_limited). In-memory, so PER NODE — fleet total is roughly cap×nodes (a runaway-loop safety valve, not fleet-wide accounting). Empty disables. ()"`
	MailSpamThresholds           string   `id:"mail-spam-thresholds" default:"suspicious=5,spam=10" desc:"Score bands for the inbound-mail spam verdict the LMTP inlet derives from an upstream Rspamd milter's headers, as \"suspicious=<score>,spam=<score>\". Sets _txc.mail.spam.verdict to clean/suspicious/spam from _txc.mail.spam.score (which is also exposed raw so txcl can band it independently). When Rspamd added no headers the score is unavailable and the verdict is \"unknown\". (suspicious=5,spam=10)"`
	TCPListenAddrs               []string `id:"tcp-listen-addrs" default:":5050" desc:"Listen addresses for the TCP head. Comma list of 'name=addr' or bare 'addr'. A named entry sets _txc.tcp.listener to that name for ingress routing (e.g. 'webhooks=:5050,iot=:5051'); a bare entry keeps the back-compat name 'default'. Every envelope also carries _txc.tcp.local.{ip,port} for rules that want to route on the raw bound port. (:5050)"`
	TCPConnectRespTimeout        string   `id:"tcp-connect-resp-timeout" default:"3s" desc:"Time that backends must accept a new connection before dropping it. (3s)"`
	TCPMaxIdleTimeout            string   `id:"tcp-max-idle-timeout" default:"5s" desc:"Max idle time between commands. May be set lower at runtime. (5s)"`
	TCPRespTimeout               string   `id:"tcp-resp-timeout" default:"10s" desc:"Max time for us to respond to command. (10s)"`
	DNSListenAddrs               []string `id:"dns-listen-addrs" default:":5354" desc:"Authoritative-DNS listen addresses (UDP+TCP bound on each). Comma list of ':port' or 'host:port'. Default :5354 — deliberately NOT :5353 (that's mDNS on macOS and clashes); the well-known DNS port 53 needs root/CAP_NET_BIND_SERVICE or a front LB. Set empty to disable the head even when 'dns' is in --personalities. (:5354)"`
	DNSRRLPerSec                 int      `id:"dns-rrl-per-sec" default:"0" desc:"Per-source-IP DNS response-rate-limit (queries/sec); over-limit queries are dropped (anti-amplification). 0 (default) disables. (0)"`
	DNSNameservers               []string `id:"dns-nameservers" default:"" desc:"Authoritative nameserver hostnames advertised in synthesized zone NS records (and printed as delegation instructions on 'txco dns zone create'). Comma list, e.g. 'ns1.txco.io,ns2.txco.io'. Empty disables NS synthesis. ()"`
	DNSEdgeIPs                   []string `id:"dns-edge-ips" default:"" desc:"Edge IPv4/IPv6 addresses synthesized as the A/AAAA target for a delegated zone's apex and per-stack hosts. Comma list, e.g. '203.0.113.10'. Empty disables A/AAAA synthesis. ()"`
	DNSMXHost                    string   `id:"dns-mx-host" default:"" desc:"Mail exchanger hostname synthesized as the MX target for delegated-zone hosts (the chassis LMTP head's public name). Empty disables MX synthesis. ()"`
	DNSMXPriority                int      `id:"dns-mx-priority" default:"10" desc:"Preference value for synthesized MX records. (10)"`
	DNSSynthTTL                  int      `id:"dns-synth-ttl" default:"60" desc:"TTL (seconds) applied to synthesized pattern records. (60)"`
	DNSSPF                       string   `id:"dns-spf" default:"" desc:"Override the apex SPF TXT synthesized for delegated mail zones. Empty (default) auto-derives 'v=spf1 ip4:<edge-ips> mx ~all' (softfail) so outbound from the relay passes. Only emitted when --dns-mx-host is set. ()"`
	DNSDMARC                     string   `id:"dns-dmarc" default:"v=DMARC1; p=none" desc:"DMARC policy TXT synthesized at _dmarc.<zone> for delegated mail zones. Default p=none (monitor, no rejection) with no rua (no automated report mailbox yet). Empty disables. Only emitted when --dns-mx-host is set. (v=DMARC1; p=none)"`
	DNSTenantZoneManagement      bool     `id:"dns-tenant-zone-management" default:"false" desc:"Escape hatch: allow tenants holding the dns:* capability to manage their OWN delegated zones + override records (and render). Default false — DNS zone management is operator-only (super-admin), since delegating zone control to tenants is a sharp edge we don't encourage. The chassis-global synthesis config (--dns-* / 'dns config set') is always super-admin regardless."`
	DNSRequireZoneVerification   bool     `id:"dns-require-zone-verification" default:"false" desc:"Require a delegated zone's NS to resolve to --dns-nameservers before it confers ANY authority (DKIM signing, verified-sender, inbound routing, authoritative serving). Default false — a created zone is trusted immediately (dev / single-operator). Set true for multi-tenant self-service: 'txco dns zone create' leaves the zone PENDING until 'txco dns zone verify' confirms the NS delegation, closing the squatting hole. (false)"`
	DNSUpdateTSIGKeyName         string   `id:"dns-update-tsig-key-name" default:"" desc:"TSIG key name authorizing RFC2136 dynamic UPDATE of _acme-challenge TXT records (lets an external ACME client, e.g. Caddy's caddy-dns/rfc2136, inject DNS-01 challenges into this authoritative server). Empty disables the UPDATE path entirely — every UPDATE is refused. Both this and --dns-update-tsig-secret must be set to enable it. ()"`
	DNSUpdateTSIGSecret          string   `id:"dns-update-tsig-secret" default:"" desc:"Base64-encoded shared secret for the --dns-update-tsig-key-name TSIG key (same value configured in the ACME client). Keep it out of shell history; prefer the env var TXCO_DNS_UPDATE_TSIG_SECRET. ()"`
	WebTLSAddr                   string   `id:"web-tls-addr" default:"" desc:"HTTPS listen address for the bundled TLS terminator (e.g. ':8443'). Empty (default) means the chassis serves plain HTTP on --web-addr and a front proxy terminates TLS. When set, the chassis terminates TLS itself, obtaining + renewing wildcard certificates for delegated zones via ACME DNS-01 against its own authoritative DNS head — requires the 'dns' personality and --acme-email. ()"`
	ACMEEmail                    string   `id:"acme-email" default:"" desc:"ACME account contact email for the bundled cert manager (recommended by CAs for expiry notices). Required when --web-tls-addr is set against a public CA. ()"`
	ACMECA                       string   `id:"acme-ca" default:"" desc:"ACME directory URL. Empty (default) uses Let's Encrypt production. Point at LE staging while testing, or a local Pebble/step-ca directory (e.g. https://localhost:14000/dir) for an offline smoke test. ()"`
	ACMECARootFile               string   `id:"acme-ca-root-file" default:"" desc:"Path to a PEM root-CA bundle to trust as the ACME CA's root, for a CA not in the system trust store (Pebble/step-ca). Empty (default) uses the system roots — correct for Let's Encrypt. ()"`
	ACMEDNSResolvers             []string `id:"acme-dns-resolvers" default:"" desc:"DNS resolvers the bundled cert manager uses for zone discovery + DNS-01 propagation checks. Empty (default) queries the zone's authoritative servers directly (correct in production). Point at this chassis's own DNS head (e.g. 127.0.0.1:5354) for an offline/localhost solve. ()"`
	CertStorageDSN               string   `id:"cert-storage-dsn" default:"" desc:"Storage backend for issued certificates + the ACME account. Empty (default) stores them on the local filesystem at --cert-storage-path (single-node). A recognised scheme (e.g. postgres://...) uses a shared backend so any node loads the same certs and issuance is serialised. ()"`
	CertStoragePath              string   `id:"cert-storage-path" default:"acme" desc:"Filesystem directory for the bundled cert/account store when --cert-storage-dsn is empty. (acme)"`
	WebAddr                      string   `id:"web-addr" default:":8080" desc:"The port to listen on for the web server (:8080)"`
	WebPass                      string   `id:"web-pass" default:"" desc:"Basic Auth password ()"`
	WebUser                      string   `id:"web-user" default:"" desc:"User for basic auth ()"`
	WebIdleTimeout               int      `id:"web-idle-timeout" default:"60" desc:"Idle timeout, seconds (60)"`
	WebReadTimeout               int      `id:"web-read-timeout" default:"15" desc:"Read timeout, seconds (15)"`
	WebWriteTimeout              int      `id:"web-write-timeout" default:"15" desc:"Write timeout, seconds (15)"`
	ContinuationLongPollMS       int      `id:"continuation-longpoll-ms" default:"12000" desc:"Max ms to hold a continuation status poll open server-side before returning 202 (adaptive long-poll). Auto-clamped to stay under web-write-timeout; 0 = legacy single-shot poll."`
	WebDebug                     string   `id:"web-debug" default:"" desc:"Debug flags: SHOW_PRIVATE_VARS, HIDE_PRIVATE_VARS"`
	WebMockHeader                bool     `id:"web-mock-header" default:"false" desc:"Honor the X-Txco-Mocks request header and map it into _txc.mocks (caller-driven mock interception). Dev convenience; leave off in production."`
	UsageEnabled                 bool     `id:"usage-enabled" default:"true" desc:"Emit one structured 'usage' log line per completed request (rid, tenant, sizes, timing, status) for downstream accounting. On by default; set --usage-enabled=false to disable."`
	UsageSink                    string   `id:"usage-sink" default:"zap" desc:"Usage sink backend: {zap}. zap folds each event into the structured 'usage' log line. usage-enabled=false disables usage entirely regardless of this. (zap)"`
	TelemetryEnabled             bool     `id:"telemetry-enabled" default:"true" desc:"Tenant telemetry: process _txc.telemetry.metrics intents at request end and export them. A tenant is only live once it sets its TELEMETRY_ENDPOINT secret; without it intents are dropped. (true)"`
	TelemetryExporter            string   `id:"telemetry-exporter" default:"otlp" desc:"Telemetry exporter backend: {otlp, log}. otlp ships OTLP/HTTP to the tenant-configured endpoint; log writes each metric as a chassis log line (dev). (otlp)"`
	BackgroundServices           string   `id:"background-services" default:"" desc:"Comma-list of long-running background services to run (chassis-owned loops, started/stopped with the controllers). Empty by default. ()"`
}

func Load() (Config, error) {
	config := Config{}

	if err := loadFromFlagsAndEnv(&config); err != nil {
		return config, err
	}

	// setup runtime with our serverid (unique per process lifetime)
	if config.ServerId == "" {
		config.ServerId = hxid.New().String()
	}

	// get our FQDN
	if config.Fqdn == "" {
		fqdn, err := fqdn.Get()
		if err != nil {
			log.Fatalf("Error getting Fqdn %v", err)
		}
		config.Fqdn = fqdn
	}

	// expand relative paths to absolute for KeyValue Config
	dir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Unable to deterimine project root for kv store")
	}
	for i, kvaddr := range config.KVStoreAddrs {
		if strings.HasPrefix(kvaddr, ".") {
			// convert to absolute path

			config.KVStoreAddrs[i] = path.Join(dir, kvaddr)
			err = os.MkdirAll(config.KVStoreAddrs[i], os.ModePerm)
			if err != nil {
				log.Fatalf("unable to make directory %s %q", config.KVStoreAddrs[i], err)
			}
		}
	}

	// repo
	if config.RepoStore == "file" {
		if len(config.RepoStoreFileDir) > 0 {
			if strings.HasPrefix(config.RepoStoreFileDir, ".") {
				// convert to absolute path
				p, _ := os.Getwd()
				config.RepoStoreFileDir = filepath.Join(p, config.RepoStoreFileDir)
			}

			err = os.MkdirAll(config.RepoStoreFileDir, os.ModePerm)

			if err != nil {
				log.Fatalf("unable to make directory %s", config.RepoStoreFileDir)
			}
		}
	}

	// artifact store (snapshot/event artifacts) — same idiom as repo store
	if config.ArtifactStore == "file" {
		if len(config.ArtifactStoreFileDir) > 0 {
			if strings.HasPrefix(config.ArtifactStoreFileDir, ".") {
				p, _ := os.Getwd()
				config.ArtifactStoreFileDir = filepath.Join(p, config.ArtifactStoreFileDir)
			}
			if err = os.MkdirAll(config.ArtifactStoreFileDir, os.ModePerm); err != nil {
				log.Fatalf("unable to make directory %s", config.ArtifactStoreFileDir)
			}
		}
	}

	// filecas store (tenant FILES/ content-addressed) — same idiom as artifact store
	if config.FileCASStore == "file" {
		if len(config.FileCASStoreFileDir) > 0 {
			if strings.HasPrefix(config.FileCASStoreFileDir, ".") {
				p, _ := os.Getwd()
				config.FileCASStoreFileDir = filepath.Join(p, config.FileCASStoreFileDir)
			}
			if err = os.MkdirAll(config.FileCASStoreFileDir, os.ModePerm); err != nil {
				log.Fatalf("unable to make directory %s", config.FileCASStoreFileDir)
			}
		}
	}

	// feed source — same idiom as artifact store
	if config.FeedSource == "file" {
		if len(config.FeedSourceFileDir) > 0 {
			if strings.HasPrefix(config.FeedSourceFileDir, ".") {
				p, _ := os.Getwd()
				config.FeedSourceFileDir = filepath.Join(p, config.FeedSourceFileDir)
			}
			if err = os.MkdirAll(config.FeedSourceFileDir, os.ModePerm); err != nil {
				log.Fatalf("unable to make directory %s", config.FeedSourceFileDir)
			}
		}
	}

	if len(config.OpMetricsRegex) > 0 {
		regexp.MustCompile(config.OpMetricsRegex)
	}

	if len(config.AdminStaticRoot) > 0 {
		if strings.HasPrefix(config.AdminStaticRoot, ".") {
			// convert to absolute path
			p, _ := os.Getwd()
			config.AdminStaticRoot = filepath.Join(p, config.AdminStaticRoot)
		}
		err = os.MkdirAll(config.AdminStaticRoot, os.ModePerm)
		if err != nil {
			log.Fatalf("unable to make directory %s", config.AdminStaticRoot)
		}
	}

	if len(config.LogOpsDir) > 0 {
		if strings.HasPrefix(config.LogOpsDir, ".") {
			// convert to absolute path
			p, _ := os.Getwd()
			config.LogOpsDir = filepath.Join(p, config.LogOpsDir)
		}
		err = os.MkdirAll(config.LogOpsDir, os.ModePerm)
		if err != nil {
			log.Fatalf("unable to make directory %s", config.LogOpsDir)
		}
	}

	// convert relative repo path to absolute
	if strings.HasPrefix(config.RepoCloneSource, "file://./") {
		p, _ := os.Getwd()
		config.RepoCloneSource = "file://" + p + config.RepoCloneSource[8:]
	}

	if len(config.DbSchemaDir) > 0 {
		// convert relative db schema to absolute
		if strings.HasPrefix(config.DbSchemaDir, ".") {
			p, _ := os.Getwd()
			config.DbSchemaDir = filepath.Join(p, config.DbSchemaDir)
		}
	}

	// convert relative db root to absolute
	if strings.HasPrefix(config.DbRoot, ".") {
		p, _ := os.Getwd()
		config.DbRoot = filepath.Join(p, config.DbRoot)
	}

	// Expand $db-root-dir, leading ./, and $env in both DSNs the same way.
	// Both DSNs (runtime + auth) flow through the same normaliser so the
	// only thing that differs is the filename portion.
	config.DbRuntimeDsn = expandDbDsn(config.DbRuntimeDsn, config.DbRoot, config.Environment)
	config.DbAuthDsn = expandDbDsn(config.DbAuthDsn, config.DbRoot, config.Environment)

	// Ensure DbRoot exists if either DSN is file-backed.
	if strings.HasPrefix(config.DbRuntimeDsn, "file:") || strings.HasPrefix(config.DbAuthDsn, "file:") {
		if err := os.MkdirAll(config.DbRoot, os.ModePerm); err != nil {
			log.Fatalf("unable to make directory %s", config.DbRoot)
		}
	}
	if strings.Contains(config.K8sNamespace, "$env") {
		config.K8sNamespace = strings.ReplaceAll(config.K8sNamespace, "$env", config.Environment)
	}

	// check that we can parse OpTimeout
	t := fmt.Sprintf("%v", config.OpTimeout)
	opTimeoutDur, err := time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse op timeout %s", config.OpTimeout)
	}

	// check that we can parse OpTimeoutMax and that the default fits under it
	t = fmt.Sprintf("%v", config.OpTimeoutMax)
	opTimeoutMaxDur, err := time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse op timeout max %s", config.OpTimeoutMax)
	}
	if opTimeoutDur > opTimeoutMaxDur {
		log.Fatalf("op-timeout (%s) must be <= op-timeout-max (%s)", config.OpTimeout, config.OpTimeoutMax)
	}

	// async-runtime-default and async-ack-timeout must parse. They are
	// deliberately NOT bounded by op-timeout-max — async ops are the
	// long-running path (see internal docs/todo-deferred-join.md).
	if _, err = time.ParseDuration(fmt.Sprintf("%v", config.AsyncRuntimeDefault)); err != nil {
		log.Fatalf("unable to parse async-runtime-default %s", config.AsyncRuntimeDefault)
	}
	if _, err = time.ParseDuration(fmt.Sprintf("%v", config.AsyncAckTimeout)); err != nil {
		log.Fatalf("unable to parse async-ack-timeout %s", config.AsyncAckTimeout)
	}
	if _, err = time.ParseDuration(fmt.Sprintf("%v", config.DeferredJoinSlack)); err != nil {
		log.Fatalf("unable to parse deferred-join-slack %s", config.DeferredJoinSlack)
	}

	// continue-after-default and continuable-timeout-default. The default
	// must satisfy `continue_after > 0` and `continue_after < timeout` so a
	// promotion can fire before the op itself errors out; flag misconfigs
	// early at boot rather than at first dispatch.
	contAfter, err := time.ParseDuration(fmt.Sprintf("%v", config.ContinueAfterDefault))
	if err != nil {
		log.Fatalf("unable to parse continue-after-default %s", config.ContinueAfterDefault)
	}
	if contAfter <= 0 {
		log.Fatalf("continue-after-default must be > 0, got %s", config.ContinueAfterDefault)
	}
	contTimeout, err := time.ParseDuration(fmt.Sprintf("%v", config.ContinuableTimeoutDefault))
	if err != nil {
		log.Fatalf("unable to parse continuable-timeout-default %s", config.ContinuableTimeoutDefault)
	}
	if contTimeout <= contAfter {
		log.Fatalf("continuable-timeout-default (%s) must be > continue-after-default (%s) — promotion would never fire",
			config.ContinuableTimeoutDefault, config.ContinueAfterDefault)
	}

	t = fmt.Sprintf("%v", config.DialTimeout)
	_, err = time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse dial timeout %s", config.DialTimeout)
	}

	t = fmt.Sprintf("%v", config.TCPConnectRespTimeout)
	_, err = time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse tcp connect resp timeout %s", config.TCPConnectRespTimeout)
	}

	// Validate the ingress-miss-action enum at boot. Silent typos
	// here would be dangerous: an operator setting --ingress-miss-
	// action=rejct (typo) would silently get fallthrough behavior
	// instead of the intended security-tightening reject mode.
	switch config.IngressMissAction {
	case "fallthrough", "reject":
		// ok
	default:
		log.Fatalf("invalid --ingress-miss-action %q: must be one of {fallthrough, reject}", config.IngressMissAction)
	}

	t = fmt.Sprintf("%v", config.TCPRespTimeout)
	_, err = time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse tcp resp timeout %s", config.TCPRespTimeout)
	}

	t = fmt.Sprintf("%v", config.TCPMaxIdleTimeout)
	_, err = time.ParseDuration(t)
	if err != nil {
		log.Fatalf("unable to parse max idle timeout %s", config.TCPMaxIdleTimeout)
	}

	// registry fixed
	if len(config.RegistryFixed) != 0 {
		for _, serviceDef := range config.RegistryFixed {
			s := strings.Split(serviceDef, ":")
			if len(s) != 3 {
				// so sorry we were looking for key:address:port
				log.Fatalf("RegistryFixed did not find key:address:port, got %s\n", config.RegistryFixed)
			}
		}
	}

	// docker tmp
	if len(config.DockerTmp) > 0 {
		if strings.HasPrefix(config.DockerTmp, ".") {
			// convert to absolute path
			p, _ := os.Getwd()
			config.DockerTmp = filepath.Join(p, config.DockerTmp)
		}
		err = os.MkdirAll(config.DockerTmp, os.ModePerm)
		if err != nil {
			log.Fatalf("unable to make directory %s", config.DockerTmp)
		}
	}

	// debug flags
	if len(config.WebDebug) != 0 {
		// if we have debug flags, set dev defaults, obeying anything set
		if strings.HasPrefix(config.Environment, "dev") && !strings.Contains(config.WebDebug, "HIDE_PRIVATE_VARS") {
			config.WebDebug = config.WebDebug + " SHOW_PRIVATE_VARS " // may duplicate but that's ok
		}
	} else if strings.HasPrefix(config.Environment, "dev") {
		// otherwise for dev, default to show
		config.WebDebug = "SHOW_PRIVATE_VARS"
	}

	// kubernetes config
	if len(config.KubeConfig) > 0 {
		if strings.HasPrefix(config.KubeConfig, ".") {
			// convert to absolute path
			p, _ := os.Getwd()
			config.KubeConfig = filepath.Join(p, config.KubeConfig)
		}
		if strings.HasPrefix(config.KubeConfig, "$USER/.kube/config") {
			home := homeDir()
			config.KubeConfig = filepath.Join(home, ".kube", "config")
		}
	}

	return config, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

// expandDbDsn applies the three substitutions shared by every DSN field:
//   - "file:$db-root-dir/..." becomes "file:<absolute db root>/..."
//   - "file:./..." becomes "file:<cwd>/..."
//   - "$env" becomes the configured environment name
func expandDbDsn(dsn, dbRoot, env string) string {
	const prefix = "file:$db-root-dir"
	if strings.HasPrefix(dsn, prefix) {
		dsn = "file:" + dbRoot + dsn[len(prefix):]
	}
	if strings.HasPrefix(dsn, "file:./") {
		p, _ := os.Getwd()
		dsn = "file:" + p + "/" + dsn[len("file:./"):]
	}
	if strings.Contains(dsn, "$env") {
		dsn = strings.ReplaceAll(dsn, "$env", env)
	}
	return dsn
}

// RedactDSN masks the password in a URL-shaped DSN so it can be logged.
// A shared-Postgres auth DSN (postgres://user:secret@host/db) carries a
// credential; file: SQLite DSNs have none and pass through unchanged.
// On any parse failure it returns a safe constant rather than risk
// leaking the raw string.
func RedactDSN(dsn string) string {
	if !strings.Contains(dsn, "://") {
		return dsn // file:… and other non-URL DSNs carry no secret
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "[redacted-dsn]"
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}

// loadFromFlagsAndEnv populates c from CLI flags and TXCO_* environment
// variables, driven by the `id`, `default`, and `desc` struct tags on Config.
// Replaces github.com/stevenroose/gonfig with the de-facto standard
// pflag + viper pair.
//
// Precedence (highest first): flag > env (TXCO_<UPPER_SNAKE>) > default.
func loadFromFlagsAndEnv(c *Config) error {
	fs := pflag.NewFlagSet("chassis", pflag.ExitOnError)
	v := viper.New()
	v.SetEnvPrefix("TXCO")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()

	t := reflect.TypeOf(*c)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		id := f.Tag.Get("id")
		if id == "" {
			continue
		}
		def := f.Tag.Get("default")
		desc := f.Tag.Get("desc")

		switch f.Type.Kind() {
		case reflect.String:
			fs.String(id, def, desc)
		case reflect.Int:
			n, _ := strconv.Atoi(def)
			fs.Int(id, n, desc)
		case reflect.Bool:
			fs.Bool(id, def == "true", desc)
		case reflect.Slice:
			if f.Type.Elem().Kind() != reflect.String {
				return fmt.Errorf("config: unsupported slice element kind %v for %s", f.Type.Elem().Kind(), id)
			}
			var defs []string
			if def != "" {
				defs = strings.Split(def, ",")
			}
			fs.StringSlice(id, defs, desc)
		default:
			return fmt.Errorf("config: unsupported field kind %v for %s", f.Type.Kind(), id)
		}

		if err := v.BindPFlag(id, fs.Lookup(id)); err != nil {
			return fmt.Errorf("config: bind %s: %w", id, err)
		}
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	val := reflect.ValueOf(c).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		id := f.Tag.Get("id")
		if id == "" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.String:
			val.Field(i).SetString(v.GetString(id))
		case reflect.Int:
			val.Field(i).SetInt(int64(v.GetInt(id)))
		case reflect.Bool:
			val.Field(i).SetBool(v.GetBool(id))
		case reflect.Slice:
			val.Field(i).Set(reflect.ValueOf(v.GetStringSlice(id)))
		}
	}

	return nil
}
