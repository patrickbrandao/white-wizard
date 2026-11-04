-- reset_counters: zeroes rate-limit state of one entity, keeping status,
-- strikes, credits, purchased and in-flight concurrency; zeroes spent.
-- KEYS: ent, tb, win, q5h, qwk, ramp.
redis.call('DEL', KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6])
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('HSET', KEYS[1], 'requests', 0, 'count', 0, 'spent', 0)
end
return 1
