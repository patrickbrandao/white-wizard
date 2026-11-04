-- sync_local: folds the consumption of the local (local_first) buckets into
-- the global rps/rpm buckets. KEYS: ent, tb. ARGV: now, used_rps, used_rpm,
-- requests_delta, rps_cap, rps_rate, rpm_cap, rpm_rate, kind, id.
-- Reply: {rps_tokens, rpm_tokens, status (0 active, 1 suspended, 2 banned),
-- until_ms, created, reason}.
local now = n(ARGV[1])
local used_rps, used_rpm, dreq = n(ARGV[2]), n(ARGV[3]), n(ARGV[4])
local rcap, rrate, mcap, mrate = n(ARGV[5]), n(ARGV[6]), n(ARGV[7]), n(ARGV[8])
local created = 0
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'kind', ARGV[9], 'id', ARGV[10], 'since', now, 'status', 'active')
  created = 1
end
if dreq ~= 0 then redis.call('HINCRBY', KEYS[1], 'requests', dreq) end
ent_ttl(KEYS[1], now)
local tb = hmap(KEYS[2])
local out = { -1, -1 }
local units = { { 'rps', rcap, rrate, used_rps }, { 'rpm', mcap, mrate, used_rpm } }
for i, u in ipairs(units) do
  if u[2] > 0 and u[3] > 0 then
    local bk = bucket(tb, u[1], now, u[2], u[3])
    bk.t = maxn(0, bk.t - u[4])
    redis.call('HSET', KEYS[2], u[1] .. ':t', f6(bk.t), u[1] .. ':ts', bk.ts, u[1] .. ':cap', fg(bk.cap), u[1] .. ':rate', fg(bk.rate))
    out[i] = fl(bk.t)
  end
end
if redis.call('PTTL', KEYS[2]) < 120000 then redis.call('PEXPIRE', KEYS[2], 120000) end
local f = redis.call('HMGET', KEYS[1], 'ban_since', 'ban_reason', 'blk_since', 'blk_reason', 'blk_until')
local st, untl, reason = 0, 0, ''
if f[1] then
  st, reason = 2, f[2] or ''
elseif f[3] and (n(f[5]) == 0 or now < n(f[5])) then
  st, untl, reason = 1, n(f[5]), f[4] or ''
end
return { out[1], out[2], st, untl, created, reason }
