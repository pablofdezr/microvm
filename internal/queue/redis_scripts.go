package queue

import "github.com/redis/go-redis/v9"

// The Lua half of RedisQueue.
//
// # The ordering trick
//
// A sorted set orders by score, and ties by member name. That is exactly the
// two-level order the queue needs -- priority first, then arrival -- provided
// both levels are encoded where the set can see them:
//
//	score  = -priority                 (negated: ZPOPMIN takes the highest priority)
//	member = <seq padded to 20>:<id>   (a tie on score falls back to member order)
//
// The sequence is zero-padded because member comparison is lexicographic: "10"
// sorts before "9", but "0000000000000000010" does not sort before
// "0000000000000000009". Twenty digits holds any uint64.
//
// The alternative -- packing priority and sequence into one float score -- is
// what a first attempt reaches for, and it is a trap: a float64 has 53 bits of
// mantissa, so a large enough sequence silently loses its low bits and two
// tasks compare equal. Ordering would then degrade under exactly the load that
// makes ordering matter. Splitting the two levels across score and member keeps
// both exact, forever.

// The padding width lives only in Lua, which is the only thing that reads it.
// It is pinned by a contract test that enqueues enough tasks to cross a digit
// boundary -- the point where an unpadded sequence would put task 10 ahead of
// task 9 and quietly stop being FIFO.
//
// luaPrelude holds what more than one script needs. Scripts are separate blobs
// to Redis, so sharing means textual concatenation -- and duplicating this
// logic instead would let the retry path a worker takes and the retry path the
// reaper takes drift apart, which is a bug nobody would find until a node died.
const luaPrelude = `
local ID_OFFSET = 22

local function idFromMember(member)
  return string.sub(member, ID_OFFSET)
end

local function pushPending(pendingKey, seqKey, notifyKey, taskKey, id, priority, doorbellCap)
  local seq = redis.call('INCR', seqKey)
  local member = string.format('%020d', seq) .. ':' .. id
  redis.call('HSET', taskKey, 'member', member, 'token', '')
  redis.call('ZADD', pendingKey, -priority, member)
  redis.call('LPUSH', notifyKey, '1')
  redis.call('LTRIM', notifyKey, 0, doorbellCap - 1)
end

-- releaseTask requeues a task that failed, or records it as failed for good
-- once its attempts run out. Returns 1 requeued, 2 failed for good, 0 unknown.
local function releaseTask(ns, pendingKey, leasedKey, seqKey, notifyKey, failedKey,
                           id, reason, nowMS, ttl, doorbellCap)
  local taskKey = ns .. ':task:' .. id
  local h = redis.call('HMGET', taskKey, 'attempt', 'priority', 'max_attempts')
  if not h[1] then return 0 end

  local attempt = tonumber(h[1])
  local priority = tonumber(h[2])
  local maxAttempts = tonumber(h[3])

  redis.call('ZREM', leasedKey, id)

  if attempt >= maxAttempts then
    redis.call('DEL', taskKey)
    local body = cjson.encode({
      error = string.format('failed after %d attempts: %s', attempt, reason),
      attempts = attempt,
      finished_at = tonumber(nowMS),
    })
    redis.call('SET', ns .. ':result:' .. id, body, 'EX', ttl)
    redis.call('INCR', failedKey)
    return 2
  end

  pushPending(pendingKey, seqKey, notifyKey, taskKey, id, priority, doorbellCap)
  return 1
end
`

// scriptEnqueue adds a task, refusing one whose ID is already in the queue.
//
// The task hash is the existence check for both states at once: it is written
// here and deleted only when the task finishes, so it is present while the task
// is pending and while it is leased. One EXISTS therefore answers "is this ID
// already queued or running?" -- the question that makes a caller-supplied ID
// an idempotency key.
var scriptEnqueue = redis.NewScript(`
local pendingKey, seqKey, notifyKey = KEYS[1], KEYS[2], KEYS[3]
local ns, id, payload = ARGV[1], ARGV[2], ARGV[3]
local priority, maxAttempts = tonumber(ARGV[4]), tonumber(ARGV[5])
local nowMS, doorbellCap = ARGV[6], tonumber(ARGV[7])
local cpu, mem = ARGV[8], ARGV[9]

local taskKey = ns .. ':task:' .. id
if redis.call('EXISTS', taskKey) == 1 then
  return 'DUP'
end

local seq = redis.call('INCR', seqKey)
local member = string.format('%020d', seq) .. ':' .. id
redis.call('HSET', taskKey,
  'payload', payload,
  'attempt', 0,
  'token', '',
  'member', member,
  'priority', priority,
  'max_attempts', maxAttempts,
  'enqueued_at', nowMS,
  'cpu', cpu,
  'mem', mem)
redis.call('ZADD', pendingKey, -priority, member)
redis.call('LPUSH', notifyKey, '1')
redis.call('LTRIM', notifyKey, 0, doorbellCap - 1)
return 'OK'
`)

// scriptLease hands back the highest-priority task that fits the caller's free
// resources, and marks it leased.
//
// Selecting and marking must be one script. As two commands, two nodes both
// take the same task -- or worse, a node takes one and dies before marking, and
// the task is simply gone with no lease to expire and nothing to bring it back.
//
// It scans pending in priority order (ZRANGE is ascending score, and score is
// -priority, so the head is the highest priority) and returns the first task
// whose cpu and mem fit availCPU/availMem. That is what makes a mix of large and
// small tasks pack instead of blocking: a task too big for this node is stepped
// over, not popped, so it stays for a node that can run it, and a smaller task
// behind it runs now. The scan is capped so a huge backlog cannot make one lease
// call walk the whole set; a task beyond the cap waits for the front to clear.
//
// The attempt count comes back as a string on purpose: Redis truncates a Lua
// number on its way out, and a count returned through float arithmetic is a
// silent rounding waiting to happen.
var scriptLease = redis.NewScript(luaPrelude + `
local pendingKey, leasedKey, reservationKey = KEYS[1], KEYS[2], KEYS[3]
local ns, expiresMS, token = ARGV[1], tonumber(ARGV[2]), ARGV[3]
local availCPU, availMem = tonumber(ARGV[4]), tonumber(ARGV[5])
local totalCPU, totalMem = tonumber(ARGV[6]), tonumber(ARGV[7])
local nodeID, nowMS, resTTL = ARGV[8], tonumber(ARGV[9]), tonumber(ARGV[10])
local scanCap = tonumber(ARGV[11])

local members = redis.call('ZRANGE', pendingKey, 0, scanCap - 1)

local function leaseAt(member, id, payload, attempt)
  local a = tonumber(attempt) + 1
  redis.call('ZREM', pendingKey, member)
  redis.call('HSET', ns .. ':task:' .. id, 'attempt', a, 'token', token)
  redis.call('ZADD', leasedKey, expiresMS, id)
  return {payload, tostring(a)}
end

-- Find the head: the first member whose task hash still exists. Stale members
-- (a member that outlived its task) are dropped as they are passed.
local headIdx, headId, headPayload, headAttempt, headCPU, headMem
for i, member in ipairs(members) do
  local id = idFromMember(member)
  local h = redis.call('HMGET', ns .. ':task:' .. id, 'payload', 'attempt', 'cpu', 'mem')
  if h[1] then
    headIdx, headId, headPayload, headAttempt = i, id, h[1], h[2]
    headCPU, headMem = tonumber(h[3]) or 0, tonumber(h[4]) or 0
    break
  else
    redis.call('ZREM', pendingKey, member)
  end
end
if not headId then return false end

-- The head fits: run it, and release its reservation if it had one.
if headCPU <= availCPU and headMem <= availMem then
  if redis.call('HGET', reservationKey, 'task') == headId then
    redis.call('DEL', reservationKey)
  end
  return leaseAt(members[headIdx], headId, headPayload, headAttempt)
end

-- The head does not fit. If another live node is already draining for it, this
-- node backfills; otherwise, if this node could run it once drained, it claims
-- the reservation and waits.
local res = redis.call('HMGET', reservationKey, 'task', 'owner', 'expiry')
local reservedElsewhere = res[1] == headId and res[2] ~= nodeID and (tonumber(res[3]) or 0) > nowMS

if not reservedElsewhere and headCPU <= totalCPU and headMem <= totalMem then
  redis.call('HSET', reservationKey, 'task', headId, 'owner', nodeID, 'expiry', nowMS + resTTL)
  return false
end

-- Backfill: the highest-priority task after the head that fits this node.
for i = headIdx + 1, #members do
  local member = members[i]
  local id = idFromMember(member)
  local h = redis.call('HMGET', ns .. ':task:' .. id, 'payload', 'attempt', 'cpu', 'mem')
  if h[1] then
    if (tonumber(h[3]) or 0) <= availCPU and (tonumber(h[4]) or 0) <= availMem then
      return leaseAt(member, id, h[1], h[2])
    end
  else
    redis.call('ZREM', pendingKey, member)
  end
end
return false
`)

// scriptExtend pushes a lease's expiry out, if the caller still owns it.
var scriptExtend = redis.NewScript(`
local leasedKey = KEYS[1]
local ns, id, token, expiresMS = ARGV[1], ARGV[2], ARGV[3], tonumber(ARGV[4])

local taskKey = ns .. ':task:' .. id
if redis.call('HGET', taskKey, 'token') ~= token then return 0 end
-- The token can still match a task the reaper has already requeued, so the
-- lease set has the final say on who is actually running it.
if redis.call('ZSCORE', leasedKey, id) == false then return 0 end

redis.call('ZADD', leasedKey, expiresMS, id)
return 1
`)

// scriptComplete records a result, if the caller still owns the lease.
var scriptComplete = redis.NewScript(`
local leasedKey, doneKey, failedKey = KEYS[1], KEYS[2], KEYS[3]
local ns, id, token, body = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local ttl, isErr = tonumber(ARGV[5]), ARGV[6]

local taskKey = ns .. ':task:' .. id
if redis.call('HGET', taskKey, 'token') ~= token then return 0 end

redis.call('ZREM', leasedKey, id)
redis.call('DEL', taskKey)
redis.call('SET', ns .. ':result:' .. id, body, 'EX', ttl)

if isErr == '1' then
  redis.call('INCR', failedKey)
else
  redis.call('INCR', doneKey)
end
return 1
`)

// scriptFail releases a lease the caller holds, retrying or failing the task.
var scriptFail = redis.NewScript(luaPrelude + `
local pendingKey, leasedKey, seqKey, notifyKey, failedKey =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local ns, id, token, reason = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local nowMS, ttl, doorbellCap = ARGV[5], tonumber(ARGV[6]), tonumber(ARGV[7])

local taskKey = ns .. ':task:' .. id
local tok = redis.call('HGET', taskKey, 'token')
-- A missing task and a mismatched token mean the same thing to the caller: it
-- does not own this any more, so it must not decide the task's fate.
if tok == false or tok ~= token then return -1 end

return releaseTask(ns, pendingKey, leasedKey, seqKey, notifyKey, failedKey,
                   id, reason, nowMS, ttl, doorbellCap)
`)

// scriptReap returns tasks whose workers stopped checking in.
//
// It takes no token: that is the point. The worker that held these leases is
// assumed dead, so there is nobody to ask.
var scriptReap = redis.NewScript(luaPrelude + `
local pendingKey, leasedKey, seqKey, notifyKey, failedKey =
  KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local ns, nowMS = ARGV[1], ARGV[2]
local ttl, doorbellCap, limit = tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5])

local expired = redis.call('ZRANGEBYSCORE', leasedKey, '-inf', nowMS, 'LIMIT', 0, limit)
local n = 0
for _, id in ipairs(expired) do
  if releaseTask(ns, pendingKey, leasedKey, seqKey, notifyKey, failedKey, id,
                 'worker stopped responding: lease expired', nowMS, ttl, doorbellCap) > 0 then
    n = n + 1
  end
end
return n
`)

// scriptStats reads the queue's depth in one round trip.
//
// The head's wait needs the task hash of whichever task is first, which is not
// knowable before reading the set -- so it is a script rather than a pipeline.
var scriptStats = redis.NewScript(luaPrelude + `
local pendingKey, leasedKey, doneKey, failedKey = KEYS[1], KEYS[2], KEYS[3], KEYS[4]
local ns = ARGV[1]

local pending = redis.call('ZCARD', pendingKey)
local leased = redis.call('ZCARD', leasedKey)
local done = tonumber(redis.call('GET', doneKey) or '0')
local failed = tonumber(redis.call('GET', failedKey) or '0')

-- The head is the next task out, so its wait is the one that says whether the
-- fleet is big enough.
local oldest = 0
local head = redis.call('ZRANGE', pendingKey, 0, 0)
if #head > 0 then
  local enqueuedAt = redis.call('HGET', ns .. ':task:' .. idFromMember(head[1]), 'enqueued_at')
  if enqueuedAt then oldest = tonumber(enqueuedAt) end
end

return {pending, leased, done, failed, oldest}
`)
