-- pc_commit: create or renew prompt-cache prefixes (cache/valkey/README.md 3.14).
-- KEYS: pc (hash), pcx (zset). ARGV: now_ms, max_entries, n, then n triples
-- hash, ttl_ms, max_life_ms (0 = no cap). Reply: entries created.
local now = n(ARGV[1])
local maxe = n(ARGV[2])
local cnt = n(ARGV[3])
local created = 0
local fresh = redis.call('EXISTS', KEYS[2]) == 0
for i = 0, cnt - 1 do
  local h = ARGV[4 + 3 * i]
  local ttl = n(ARGV[5 + 3 * i])
  local life = n(ARGV[6 + 3 * i])
  local old = redis.call('ZSCORE', KEYS[2], h)
  local exp, deadline
  if old and n(old) > now then
    local v = redis.call('HGET', KEYS[1], h) or '0:0'
    local sep = string.find(v, ':', 1, true)
    local tOld, dl = 0, 0
    if sep then
      tOld = n(string.sub(v, 1, sep - 1))
      dl = n(string.sub(v, sep + 1))
    end
    ttl = maxn(tOld, ttl)
    deadline = dl
    exp = maxn(n(old), now + ttl)
  else
    created = created + 1
    deadline = 0
    if life > 0 then deadline = now + life end
    exp = now + ttl
  end
  if deadline > 0 and exp > deadline then exp = deadline end
  redis.call('HSET', KEYS[1], h, fl(ttl) .. ':' .. fl(deadline))
  redis.call('ZADD', KEYS[2], fl(exp), h)
end
-- Prune up to 64 expired entries, then the excess closest to expiring.
local dead = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now, 'LIMIT', 0, 64)
if #dead > 0 then
  redis.call('ZREM', KEYS[2], unpack(dead))
  redis.call('HDEL', KEYS[1], unpack(dead))
end
local over = redis.call('ZCARD', KEYS[2]) - maxe
if over > 0 then
  local pop = redis.call('ZPOPMIN', KEYS[2], over)
  for i = 1, #pop, 2 do redis.call('HDEL', KEYS[1], pop[i]) end
end
local top = redis.call('ZRANGE', KEYS[2], -1, -1, 'WITHSCORES')
if #top == 0 then
  redis.call('DEL', KEYS[1], KEYS[2])
  return created
end
local ttlKey = fl(n(top[2]) - now + 1000)
for _, k in ipairs(KEYS) do
  if fresh or redis.call('PTTL', k) < 0 then
    redis.call('PEXPIRE', k, ttlKey)
  else
    redis.call('PEXPIRE', k, ttlKey, 'GT')
  end
end
return created
