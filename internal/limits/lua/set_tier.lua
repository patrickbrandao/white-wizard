-- set_tier: display field tier of ent (cache/valkey/README.md 2.3).
-- KEYS: ent. ARGV: tier ("" = HDEL), now, kind, id. Reply: created (0/1).
local now = n(ARGV[2])
local created = 0
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'kind', ARGV[3], 'id', ARGV[4], 'since', now, 'status', 'active')
  created = 1
end
if ARGV[1] == '' then
  redis.call('HDEL', KEYS[1], 'tier')
else
  redis.call('HSET', KEYS[1], 'tier', ARGV[1])
end
ent_ttl(KEYS[1], now)
return created
