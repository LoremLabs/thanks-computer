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

// Command Line Flags / Environment variables to configure runtime environment
type Config struct {
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
	SecretMasterKeyPath          string   `id:"secret-master-key" default:"./chassis/data/secrets/txco-master.key" desc:"Path to the host-local master-key file for the per-tenant secret store. Auto-minted on first boot if absent (same convention as the runtime DB). File is 32 bytes with 0600 perms. Set to empty to disable the feature entirely (library / embedder opt-out)."`
	DebugBreakpoints             bool     `id:"debug-breakpoints" default:"false" desc:"Enable breakpoint debugging: inlet stamps _txc.flag_breakpoint=true on every event and reads ?_txc.break=<scope> from HTTP query strings. NEVER enable in production."`
	DebugPrivate                 bool     `id:"debug-private" default:"false" desc:"Inlet stamps _txc.flag_private=true on every event. When set, the response to the client keeps private (underscore-prefixed) fields like _txc that would otherwise be stripped. Useful for development; leave off in production."`
	TraceMode                    string   `id:"trace-mode" default:"off" desc:"Request tracing: {off, summary, full} (off). Writes per-request artifacts under --trace-dir."`
	TraceStore                   string   `id:"trace-store" default:"file" desc:"Trace sink + reader backend: {noop, file} (file). An out-of-tree backend (e.g. a queue/object-store shipper for a separate-machine admin) self-registers and is selected here. trace-mode=off forces noop regardless."`
	TraceDir                     string   `id:"trace-dir" default:"./data/trace" desc:"Directory for trace artifacts when --trace-mode != off, for the file backend (./data/trace)"`
	TraceAsync                   bool     `id:"trace-async" default:"false" desc:"Buffer trace writes through an async worker so the request path never blocks on disk I/O. Recommended in production when tracing is on."`
	TraceBufferSize              int      `id:"trace-buffer-size" default:"1024" desc:"Size of the async trace event queue. When full, additional events are dropped (request path is never blocked). Applies only with --trace-async (1024)."`
	TraceBodyCapBytes            int      `id:"trace-body-cap-bytes" default:"65536" desc:"Maximum bytes kept per body (request payload, step in/out, final response) when --trace-async is on. Larger bodies are truncated; meta.json records the original size (65536, ~64KB)."`
	TraceStreamLongPollMS        int      `id:"trace-stream-longpoll-ms" default:"30000" desc:"Maximum ms to hold a /traces/stream long-poll request open server-side before returning 202 so the client can re-poll with the same cursor (30000). Auto-clamped to stay under the web-write-timeout. Only meaningful when the trace store registers a live-stream Armable (e.g. the NATS overlay)."`
	TraceStreamRingSize          int      `id:"trace-stream-ring-size" default:"1024" desc:"Per-subscription buffer hint for live trace streams. Backends may clamp; oversubscribed buffers drop the oldest events (best-effort live tail, not durable replay) (1024)."`
	CronPeriod                   int      `id:"cron-period" default:"60" desc:"Seconds between cron ticks, seconds (60)"`
	DbRuntimeDsn                 string   `id:"db-runtime-dsn" default:"file:$db-root-dir/runtime-$env.db" desc:"DSN for runtime database — ops, stacks, versions, files, tenants (file:./chassis/data/db/runtime-$env.db)"`
	DbAuthDsn                    string   `id:"db-auth-dsn" default:"file:$db-root-dir/auth-$env.db" desc:"DSN for auth database — actors, keys, memberships, invitations, browser sessions. Only opened when the admin personality is active. Default is a local SQLite file (file:./chassis/data/db/auth-$env.db); a postgres://… URL selects a shared Postgres store so an HA control plane's replicas share identity/sessions (driver provided by the SaaS overlay)."`
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
	StructuredHostSuffix         string   `id:"structured-host-suffix" default:"" desc:"When set (e.g. '.stacks.example.com'), the chassis auto-mints one tenant_hostnames row per activated stack at '<stack>-<rand><suffix>' so a freshly-applied stack is reachable with no manual binding. Empty (default) disables minting — open-core/embedders unchanged. txco dev injects '.localhost' for zero-flag local use."`
	VerifyAllowPrivateAddresses  bool     `id:"verify-allow-private-addresses" default:"false" desc:"When true, the HTTP-01 hostname verifier accepts hostnames that resolve to private/loopback/link-local IPs. Default false (production-safe, SSRF defense). txco dev flips it true so localhost-style workflows function."`
	IngressMissAction            string   `id:"ingress-miss-action" default:"fallthrough" desc:"What happens when no tenant_hostnames row matches the incoming request: 'fallthrough' (default) dispatches to boot/%/0 so operator-authored boot rules can catch-all; 'reject' returns a clean HTTP 404 without invoking the processor. Set 'reject' in production deployments that route everything via tenant_hostnames."`
	K8sNamespace                 string   `id:"k8s-namespace" default:"default" desc:"Kubernetes Namespace (default)"`
	KubeConfig                   string   `id:"kube-config" default:"$USER/.kube/config" desc:"What path to use for Kubernetes Config? ($USER/.kube/config)"`
	KubeCheckInCluster           bool     `id:"kube-check-incluster" default:"true" desc:"Use to prefer in cluster service account over kube config. Tests if we're running inside a cluster? (true)"`
	KVStore                      string   `id:"kvstore" default:"boltdb" desc:"Which Key/Value Store to use? {boltdb, etcd} (boltdb)"`
	KVStoreAddrs                 []string `id:"kvstore-addrs" default:"./chassis/data/kv" desc:"List of KVStore addresses, if appropriate. (./chassis/data/kv)"`
	KVStoreBucket                string   `id:"kvstore-bucket" default:"txco" desc:"KVStore bucket (txco)"`
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
	AsyncRuntimeDefault          string   `id:"async-runtime-default" default:"10m" desc:"Default runtime budget for an async op (WITH mode=async) when WITH timeout is omitted; seeds the continuation's expiry. Not capped by op-timeout-max. (10m)"`
	AsyncAckTimeout              string   `id:"async-ack-timeout" default:"5s" desc:"How long to wait for an async worker's 202 handoff ack (not the worker's runtime). (5s)"`
	ContinueAfterDefault         string   `id:"continue-after-default" default:"5s" desc:"Default deadline before a WITH mode=continuable op promotes from sync to continuation. Overridable per-op via WITH continue_after. Must be > 0 and < the op's effective timeout. (5s)"`
	ContinuableTimeoutDefault    string   `id:"continuable-timeout-default" default:"10m" desc:"Default runtime budget for a WITH mode=continuable op when WITH timeout is omitted. Bounds the upstream work both pre- and post-promotion. Not capped by op-timeout-max. (10m)"`
	DeferredJoinSlack            string   `id:"deferred-join-slack" default:"60s" desc:"Flat pad added to a deferred-join op's runtime budget when computing the run's reap deadline, covering downstream synchronous scopes. (60s)"`
	ComputeMaxMemoryMB           int      `id:"compute-max-memory-mb" default:"32" desc:"Per-invocation memory cap for a sandboxed compute (op://) in MB. (32)"`
	ComputeMaxWall               string   `id:"compute-max-wall" default:"250ms" desc:"Per-invocation wall-clock cap for a sandboxed compute (op://); the guest is killed if it exceeds this. (250ms)"`
	Personalities                string   `id:"personalities" default:"cron,tcp,web,admin" desc:"Head types to start. Comma delimited. {cron,tcp,web,admin,lmtp,sweep,dns} (cron,tcp,web,admin). lmtp, sweep and dns are opt-in."`
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
	SnapshotBootstrapRef         string   `id:"snapshot-bootstrap-ref" default:"" desc:"If set AND the runtime DB is fresh, fetch this artifact ref and bootstrap-restore it before serving. Empty (default) = no bootstrap."`
	FeedSource                   string   `id:"feed-source" default:"nop" desc:"Control-event feed source: {nop, file}. nop (default) disables the applier; single-node unchanged."`
	FeedSourceFileDir            string   `id:"feed-source-file-dir" default:"./chassis/data/feed" desc:"Root directory for the file feed source (./chassis/data/feed)"`
	FeedPollPeriod               int      `id:"feed-poll-period" default:"15" desc:"Seconds between control-event feed polls; applies when feed-source != nop (15)"`
	FeedSink                     string   `id:"feed-sink" default:"nop" desc:"Control-event feed sink (producer): {nop, file}. nop (default) means admin mutations stay local; no events emitted."`
	FeedSinkBatchSize            int      `id:"feed-sink-batch-size" default:"64" desc:"Max outbox rows drained per pump tick when feed-sink != nop (64)"`
	EgressPolicy                 string   `id:"egress-policy" default:"open" desc:"Outbound op dial policy: {open, private}. open (default) allows any address; private blocks loopback/private/link-local/CGNAT/cloud-metadata and any egress-deny-cidrs."`
	EgressDenyCIDRs              []string `id:"egress-deny-cidrs" default:"" desc:"Extra CIDRs the 'private' egress policy also blocks (comma-separated); for deployment-specific internal ranges. ()"`
	EgressAllowCIDRs             []string `id:"egress-allow-cidrs" default:"" desc:"CIDRs the 'private' egress policy allows even if otherwise blocked (comma-separated); explicit escape hatch for a trusted internal op endpoint. ()"`
	SystemOpstacksDir            string   `id:"system-opstacks-dir" default:"" desc:"Optional workspace dir containing an OPS/ tree whose _-prefixed stacks (OPS/_sys/...) overlay the embedded system default. Empty uses the embedded default only (txco serve). txco dev points this at the workspace."`
	SystemOpstacksWatch          bool     `id:"system-opstacks-watch" default:"false" desc:"Watch the system-opstacks dir's OPS/ tree and hot-recompile on change. Off for serve (static after boot); txco dev enables it."`
	ServerId                     string   `id:"sid" default:"" desc:"Set the Server Id ()"`
	LMTPListenAddrs              []string `id:"lmtp-listen-addrs" default:":2424" desc:"LMTP listen addresses. Comma list of 'unix:/path' or ':port'. Default :2424 (mirrors the chassis convention of high-port defaults; the well-known LMTP port is 24 but that needs root). Set to empty to explicitly disable the head even when 'lmtp' is in --personalities. (:2424)"`
	LMTPMaxMsgBytes              int      `id:"lmtp-max-msg-bytes" default:"26214400" desc:"Max accepted DATA message size in bytes. Postfix rejects with 552 on overflow. (26214400 ~= 25 MiB)"`
	LMTPMaxRecipients            int      `id:"lmtp-max-recipients" default:"50" desc:"Max RCPT TO addresses per LMTP transaction. (50)"`
	LMTPReadTimeout              string   `id:"lmtp-read-timeout" default:"30s" desc:"Per-command read timeout for the LMTP listener. (30s)"`
	LMTPDataTimeout              string   `id:"lmtp-data-timeout" default:"60s" desc:"DATA phase read timeout for the LMTP listener. (60s)"`
	LMTPRespTimeout              string   `id:"lmtp-resp-timeout" default:"30s" desc:"Pipeline response timeout (envelope dispatch → rule verdict) for an LMTP delivery. (30s)"`
	LMTPHostname                 string   `id:"lmtp-hostname" default:"" desc:"Greeting hostname for the LMTP server. Empty (default) uses os.Hostname(). ()"`
	LMTPDefaultHosts             []string `id:"lmtp-default-hosts" default:"" desc:"Comma list of hosts the chassis answers Strategy A on (tenant.stack[+mod]@<host> parses to <tenant>/<stack>). Empty (default) disables Strategy A. Multiple hosts allowed for operators running several MX-receiving names. ()"`
	TCPListenAddrs               []string `id:"tcp-listen-addrs" default:":5050" desc:"Listen addresses for TCP server Ex: :5050 (:5050)"`
	TCPConnectRespTimeout        string   `id:"tcp-connect-resp-timeout" default:"3s" desc:"Time that backends must accept a new connection before dropping it. (3s)"`
	TCPMaxIdleTimeout            string   `id:"tcp-max-idle-timeout" default:"5s" desc:"Max idle time between commands. May be set lower at runtime. (5s)"`
	TCPRespTimeout               string   `id:"tcp-resp-timeout" default:"10s" desc:"Max time for us to respond to command. (10s)"`
	DNSListenAddrs               []string `id:"dns-listen-addrs" default:":5353" desc:"Authoritative-DNS listen addresses (UDP+TCP bound on each). Comma list of ':port' or 'host:port'. Default :5353 (chassis high-port convention; the well-known DNS port 53 needs root/CAP_NET_BIND_SERVICE or a front LB). Set empty to disable the head even when 'dns' is in --personalities. (:5353)"`
	DNSRRLPerSec                 int      `id:"dns-rrl-per-sec" default:"0" desc:"Per-source-IP DNS response-rate-limit (queries/sec); over-limit queries are dropped (anti-amplification). 0 (default) disables. (0)"`
	DNSNameservers               []string `id:"dns-nameservers" default:"" desc:"Authoritative nameserver hostnames advertised in synthesized zone NS records (and printed as delegation instructions on 'txco dns zone create'). Comma list, e.g. 'ns1.txco.io,ns2.txco.io'. Empty disables NS synthesis. ()"`
	DNSEdgeIPs                   []string `id:"dns-edge-ips" default:"" desc:"Edge IPv4/IPv6 addresses synthesized as the A/AAAA target for a delegated zone's apex and per-stack hosts. Comma list, e.g. '203.0.113.10'. Empty disables A/AAAA synthesis. ()"`
	DNSMXHost                    string   `id:"dns-mx-host" default:"" desc:"Mail exchanger hostname synthesized as the MX target for delegated-zone hosts (the chassis LMTP head's public name). Empty disables MX synthesis. ()"`
	DNSMXPriority                int      `id:"dns-mx-priority" default:"10" desc:"Preference value for synthesized MX records. (10)"`
	DNSSynthTTL                  int      `id:"dns-synth-ttl" default:"60" desc:"TTL (seconds) applied to synthesized pattern records. (60)"`
	DNSTenantZoneManagement      bool     `id:"dns-tenant-zone-management" default:"false" desc:"Escape hatch: allow tenants holding the dns:* capability to manage their OWN delegated zones + override records (and render). Default false — DNS zone management is operator-only (super-admin), since delegating zone control to tenants is a sharp edge we don't encourage. The chassis-global synthesis config (--dns-* / 'dns config set') is always super-admin regardless."`
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
