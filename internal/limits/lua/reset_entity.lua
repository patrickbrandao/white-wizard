-- reset_entity: ResetEntity of one entity (cache/valkey/README.md 3.9).
-- KEYS: ent, tb, win, q5h, qwk, ramp, conc, strikes, esc.
-- ARGV: counters (0/1), strikes (0/1). Never creates ent; keeps ban_*,
-- blk_*, credits, purchased, tier and status; counters zeroes spent;
-- strikes also drops the escalation record (task 258).
local exists = redis.call('EXISTS', KEYS[1]) == 1
if ARGV[1] == '1' then
  redis.call('DEL', KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
  if exists then redis.call('HSET', KEYS[1], 'requests', 0, 'count', 0, 'spent', 0) end
end
if ARGV[2] == '1' then
  redis.call('DEL', KEYS[8], KEYS[9])
  if exists then redis.call('HSET', KEYS[1], 'strikes', 0) end
end
return 1
