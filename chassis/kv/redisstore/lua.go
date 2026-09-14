package redisstore

// Command selectors for casScript (ARGV[1]).
const (
	cmdCAS = "cas"
	cmdCAD = "cad"
)

// casScript implements compare-and-swap / compare-and-delete atomically
// server-side. Derived from github.com/kvtools/redis v1.2.0 (Apache-2.0),
// with the comparison changed from the KVPair "LastIndex" JSON field to the
// RAW STORED BYTES: the chassis stores raw values, which never carry
// LastIndex, so upstream's cjson compare was nil==nil — vacuously true — and
// the "atomic" swap raced. Byte equality against the previous value is the
// compare the chassis/kv wrapper actually needs (it re-reads and retries on
// ErrKeyModified), and it needs no cjson.
//
// Scripts execute atomically in redis, so get-compare-set here is race-free.
// The error strings are load-bearing: runScript maps them to
// store.ErrKeyNotFound / store.ErrKeyModified by substring match.
//
// KEYS/ARGV must be treated as READONLY: Upstash's Lua sandbox rejects
// mutation ("Attempt to modify a readonly table"), unlike real Redis. That is
// why the upstream table.remove(ARGV)/unpack dispatch was replaced with
// explicit fixed-arity indexing.
const casScript = `
if #KEYS > 0 then error('No Keys should be provided') end
if #ARGV <= 0 then error('ARGV should be provided') end

local command_name = assert(ARGV[1], 'Must provide a command')

local setex = function(key, val, ex)
    if ex == "0" then
        return redis.call('set', key, val)
    end
    return redis.call('set', key, val, 'ex', ex)
end

-- cas(key, old, new, ttlSec): swap to $new only if the stored bytes == $old.
local cas = function(key, old, new, ttl)
    local cur = redis.call('get', key)
    if cur == false then
        error("redis: key is not found")
    end
    if cur ~= old then
        error("redis: value has been changed")
    end
    setex(key, new, ttl)
    return "OK"
end

-- cad(key, old): delete only if the stored bytes == $old.
local cad = function(key, old)
    local cur = redis.call('get', key)
    if cur == false then
        error("redis: key is not found")
    end
    if cur ~= old then
        error("redis: value has been changed")
    end
    redis.call('del', key)
    return "OK"
end

if command_name == 'cas' then
    return cas(ARGV[2], ARGV[3], ARGV[4], ARGV[5])
end
if command_name == 'cad' then
    return cad(ARGV[2], ARGV[3])
end
error('Unknown command ' .. command_name)
`

// msetScript sets every KEYS[i] to ARGV[2i-1] with an expiry of ARGV[2i]
// seconds ("0" = persistent) — kv/mset's batch as ONE script. Redis runs a
// script without interleaving any other command, so no reader ever sees part
// of a batch. Redis does not roll back a script that errors midway, but these
// SETs cannot fail on their arguments (SET overwrites a key of any type, and
// the arity is checked before the first write), so a partial batch would take
// the server itself failing mid-script. KEYS/ARGV are only read, never
// modified — see casScript on Upstash's read-only sandbox.
const msetScript = `
if #ARGV ~= 2 * #KEYS then error('mset: want a value and a ttl per key') end
for i = 1, #KEYS do
    local ex = ARGV[2 * i]
    if ex == "0" then
        redis.call('set', KEYS[i], ARGV[2 * i - 1])
    else
        redis.call('set', KEYS[i], ARGV[2 * i - 1], 'ex', ex)
    end
end
return #KEYS
`
