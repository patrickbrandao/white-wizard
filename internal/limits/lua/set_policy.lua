-- set_policy: one field of cfg:policies plus the version, atomically
-- (cache/valkey/README.md 2.3 and 3.12; the wwz writes only ent:<tipo>:<id>).
-- KEYS: cfg:policies. ARGV: field, value ("" = HDEL), cas (0/1), expected
-- (with cas = 1: the value the field must still hold; "" = absent).
-- Reply: new version, or 0 when cas = 1 and the field changed (nothing
-- written).
if ARGV[3] == '1' then
  local cur = redis.call('HGET', KEYS[1], ARGV[1])
  if not cur then cur = '' end
  if cur ~= (ARGV[4] or '') then return 0 end
end
if ARGV[2] == '' then
  redis.call('HDEL', KEYS[1], ARGV[1])
else
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
end
return redis.call('HINCRBY', KEYS[1], 'version', 1)
