-- release: gives back what check_entity took when another entity of the same
-- request denied (cluster). KEYS: 7 keys. ARGV: now, then the 10 taken
-- values (rps, rpm_tb, tpm_tb, itpm_tb, rpm_win, tpm_win, rpd, tpd, ramp, conc).
local now = n(ARGV[1])
local function give(u, v)
  if v <= 0 then return end
  local f = redis.call('HMGET', KEYS[2], u .. ':t', u .. ':cap')
  if not f[1] then return end
  redis.call('HSET', KEYS[2], u .. ':t', f6(minn(n(f[2]), n(f[1]) + v)))
end
local function unwin(w, len, v)
  if v <= 0 then return end
  local f = redis.call('HMGET', KEYS[3], w .. ':s', w .. ':c')
  if not f[1] then return end
  local s = n(f[1])
  if now < s or now - s >= len then return end
  redis.call('HSET', KEYS[3], w .. ':c', maxn(0, n(f[2]) - v))
end
give('rps', n(ARGV[2]))
give('rpm', n(ARGV[3]))
give('tpm', n(ARGV[4]))
give('itpm', n(ARGV[5]))
unwin('rpm', MIN, n(ARGV[6]))
unwin('tpm', MIN, n(ARGV[7]))
unwin('rpd', DAY, n(ARGV[8]))
unwin('tpd', DAY, n(ARGV[9]))
local ramp = n(ARGV[10])
if ramp > 0 then
  local f = tostring(fl(now / MIN) * MIN)
  local v = n(redis.call('HGET', KEYS[6], f)) - ramp
  if v > 0 then redis.call('HSET', KEYS[6], f, v) else redis.call('HDEL', KEYS[6], f) end
end
if n(ARGV[11]) > 0 then
  local c = n(redis.call('GET', KEYS[7]))
  if c > 0 then
    redis.call('DECR', KEYS[7])
    redis.call('PEXPIRE', KEYS[7], 120000)
  end
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  local r = n(redis.call('HGET', KEYS[1], 'requests'))
  if r > 0 then redis.call('HINCRBY', KEYS[1], 'requests', -1) end
end
return 1
