-- finish: per-entity adjustments after the response (both modes).
-- KEYS: ent, tb, win, q5h, qwk, conc. ARGV: now, conc_flag, give (unused
-- reserved output tokens), otpm, actual_output, cost_micro, quota_flag,
-- month_start, tpm_mode (0 none, 1 token bucket, 2 fixed window), tpd_flag,
-- kind, id, quota_micro (what the quota windows are debited by; -1 or absent
-- = cost_micro; ADR 0072), day_start, week_start, free_flag (task 258).
-- Reply: {created, has_credits, credits, requests, since, purchased, spent}
-- (microdollars, since in ms; README 3.5, ADR 0051).
local now = n(ARGV[1])
local give, otpm, out, cost = n(ARGV[3]), n(ARGV[4]), n(ARGV[5]), n(ARGV[6])
local ms, tpm_mode, tpd_flag = n(ARGV[8]), n(ARGV[9]), n(ARGV[10])
local q = tonumber(ARGV[13]) or -1
if q < 0 then q = cost end
local ds, ws = n(ARGV[14]), n(ARGV[15])
-- madd adds v to the calendar window w of win that starts at start (month,
-- UTC day or UTC week); a later start resets it, an earlier one never does.
local function madd(w, start, v)
  local f = redis.call('HMGET', KEYS[3], w .. ':s', w .. ':c')
  local s, c = tonumber(f[1]), n(f[2])
  if s == nil or start > s then s, c = start, 0 end
  redis.call('HSET', KEYS[3], w .. ':s', s, w .. ':c', c + v)
end
local created = 0
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'kind', ARGV[11], 'id', ARGV[12], 'since', now, 'status', 'active')
  created = 1
end
if n(ARGV[2]) == 1 then
  local c = n(redis.call('GET', KEYS[6]))
  if c > 0 then
    redis.call('DECR', KEYS[6])
    redis.call('PEXPIRE', KEYS[6], 120000)
  end
end
local function refund(w, len, v)
  local f = redis.call('HMGET', KEYS[3], w .. ':s', w .. ':c')
  if not f[1] then return end
  local s = n(f[1])
  if now < s or now - s >= len then return end
  redis.call('HSET', KEYS[3], w .. ':c', maxn(0, n(f[2]) - v))
end
if give > 0 then
  if tpm_mode == 1 then
    local f = redis.call('HMGET', KEYS[2], 'tpm:t', 'tpm:cap')
    if f[1] then redis.call('HSET', KEYS[2], 'tpm:t', f6(minn(n(f[2]), n(f[1]) + give))) end
  elseif tpm_mode == 2 then
    refund('tpm', MIN, give)
  end
  if tpd_flag == 1 then refund('tpd', DAY, give) end
end
if otpm > 0 then
  local bk = bucket(hmap(KEYS[2]), 'otpm', now, otpm, otpm / 60)
  if bk.t >= out then bk.t = bk.t - out end
  redis.call('HSET', KEYS[2], 'otpm:t', f6(bk.t), 'otpm:ts', bk.ts, 'otpm:cap', fg(bk.cap), 'otpm:rate', fg(bk.rate))
  if redis.call('PTTL', KEYS[2]) < 120000 then redis.call('PEXPIRE', KEYS[2], 120000) end
end
if n(ARGV[16]) == 1 then
  madd('free', ds, 1)
  redis.call('PEXPIRE', KEYS[3], WIN_TTL)
end
if cost > 0 then
  madd('spend', ms, cost)
  madd('sday', ds, cost)
  madd('swk', ws, cost)
  redis.call('HINCRBY', KEYS[1], 'spent', cost)
  if redis.call('HEXISTS', KEYS[1], 'credits') == 1 then
    redis.call('HINCRBY', KEYS[1], 'credits', -cost)
  end
  redis.call('PEXPIRE', KEYS[3], WIN_TTL)
end
if q > 0 and n(ARGV[7]) == 1 then
  local m = fl(now / MIN) * MIN
  local h = fl(now / HOUR) * HOUR
  redis.call('HINCRBY', KEYS[4], m, q)
  redis.call('PEXPIRE', KEYS[4], 5 * HOUR + MIN)
  redis.call('HINCRBY', KEYS[5], h, q)
  redis.call('PEXPIRE', KEYS[5], 7 * DAY + HOUR)
  madd('qmo', ms, q)
  redis.call('PEXPIRE', KEYS[3], WIN_TTL)
end
ent_ttl(KEYS[1], now)
local f = redis.call('HMGET', KEYS[1], 'credits', 'requests', 'since', 'purchased', 'spent')
local has = 0
if f[1] then has = 1 end
return { created, has, n(f[1]), n(f[2]), n(f[3]), n(f[4]), n(f[5]) }
