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
const casScript = `
if #KEYS > 0 then error('No Keys should be provided') end
if #ARGV <= 0 then error('ARGV should be provided') end

local command_name = assert(table.remove(ARGV, 1), 'Must provide a command')

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

local Launcher = {
    cas = cas,
    cad = cad
}

local command = assert(Launcher[command_name], 'Unknown command ' .. command_name)
return command(unpack(ARGV))
`
