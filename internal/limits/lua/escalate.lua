-- escalate: the escalation record of the abuse ladder (task 258).
-- KEYS: esc. ARGV: escalated (0/1), ttl_ms. Counts one strike and, when
-- escalated, one escalation; renews the TTL. Reply: {level, strikes}.
local s = redis.call('HINCRBY', KEYS[1], 'strikes', 1)
local l
if ARGV[1] == '1' then
  l = redis.call('HINCRBY', KEYS[1], 'level', 1)
else
  l = n(redis.call('HGET', KEYS[1], 'level'))
end
redis.call('PEXPIRE', KEYS[1], fl(n(ARGV[2])))
return { l, s }
