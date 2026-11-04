-- strike: records a violation in the rolling strike set.
-- KEYS: strikes, ent. ARGV: now, ttl_ms, member, kind, id. Reply: {n, created}.
local now, ttl = n(ARGV[1]), n(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. tostring(now - ttl))
redis.call('ZADD', KEYS[1], now, ARGV[3])
local cnt = redis.call('ZCARD', KEYS[1])
redis.call('PEXPIRE', KEYS[1], fl(ttl))
local created = 0
if redis.call('EXISTS', KEYS[2]) == 0 then
  redis.call('HSET', KEYS[2], 'kind', ARGV[4], 'id', ARGV[5], 'since', now, 'status', 'active')
  created = 1
end
redis.call('HSET', KEYS[2], 'strikes', cnt)
ent_ttl(KEYS[2], now)
return { cnt, created }
