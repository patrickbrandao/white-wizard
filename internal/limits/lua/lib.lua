-- lib.lua: per-entity evaluation shared by check, check_entity and release
-- (cache/valkey/README.md, section 3). Concatenated in front of each script.
-- Semantics follow internal/limits/memory.go. Every number returned to the
-- client is an integer (math.floor).

local MIN, HOUR, DAY = 60000, 3600000, 86400000
local ENT_TTL = 30 * DAY
local WIN_TTL = 32 * DAY

local function n(x) return tonumber(x) or 0 end
local function f6(x) return string.format('%.6f', x) end
local function fg(x) return string.format('%.17g', x) end
local function fl(x) return math.floor(x) end
local function maxn(a, b) if a > b then return a end return b end
local function minn(a, b) if a < b then return a end return b end

local function hmap(key)
  local r = redis.call('HGETALL', key)
  local m = {}
  for i = 1, #r, 2 do m[r[i]] = r[i + 1] end
  return m
end

-- ent_ttl: no expiry while banned, indefinitely blocked or holding credits;
-- otherwise 30 days (or until the end of a longer block plus a day).
local function ent_ttl(key, now)
  local f = redis.call('HMGET', key, 'ban_since', 'credits', 'blk_since', 'blk_until')
  if f[1] or f[2] or (f[3] and n(f[4]) == 0) then
    redis.call('PERSIST', key)
    return
  end
  local ttl = ENT_TTL
  if f[3] then ttl = maxn(ttl, n(f[4]) - now + DAY) end
  redis.call('PEXPIRE', key, fl(ttl))
end

-- effective status of an entity hash at now.
local function status_of(key, now)
  local f = redis.call('HMGET', key, 'ban_since', 'blk_since', 'blk_until', 'credits')
  if f[1] then return 'banned' end
  if f[2] and (n(f[3]) == 0 or now < n(f[3])) then return 'suspended' end
  if f[4] and n(f[4]) < 0 then return 'limited' end
  return 'active'
end

-- bucket reads a token bucket from the tb map and refills it at now
-- (memory.go bucket.refill).
local function bucket(tb, u, now, cap, rate)
  local t, ts, c0, r0 = tonumber(tb[u .. ':t']), tonumber(tb[u .. ':ts']), tonumber(tb[u .. ':cap']), tonumber(tb[u .. ':rate'])
  if t == nil or ts == nil then
    return { t = cap, ts = now, cap = cap, rate = rate }
  end
  if c0 ~= cap or r0 ~= rate then
    if t > cap then t = cap end
  end
  if now > ts then
    t = minn(cap, t + (now - ts) / 1000 * rate)
    ts = now
  end
  return { t = t, ts = ts, cap = cap, rate = rate }
end

local function reset_ms(bk)
  if bk.rate <= 0 then return 0 end
  return (bk.cap - bk.t) / bk.rate * 1000
end

-- fixed window (memory.go window.add rule): restart when now - start >= len.
local function fwin(win, w, now, len)
  local s, c = tonumber(win[w .. ':s']), n(win[w .. ':c'])
  if s == nil or now - s >= len then
    s, c = fl(now / len) * len, 0
  end
  return s, c
end

-- month window: never moves backwards (memory.go monthWindow).
local function mwin(win, w, ms)
  local s, c = tonumber(win[w .. ':s']), n(win[w .. ':c'])
  if s == nil or ms > s then return ms, 0 end
  return s, c
end

-- sliding buckets: sum of live buckets, oldest live start and dead fields.
local function slide(m, now, span, width)
  local sum, oldest, dead = 0, nil, {}
  for f, v in pairs(m) do
    local s = tonumber(f)
    if s == nil or s + width <= now - span then
      dead[#dead + 1] = f
    else
      sum = sum + n(v)
      if oldest == nil or s < oldest then oldest = s end
    end
  end
  return sum, oldest, dead
end

-- since sums the buckets of m that start at or after start (a calendar
-- window that began then) and lists the older ones as dead.
local function since(m, start)
  local sum, dead = 0, {}
  for f, v in pairs(m) do
    local s = tonumber(f)
    if s == nil or s < start then
      dead[#dead + 1] = f
    else
      sum = sum + n(v)
    end
  end
  return sum, dead
end

-- otpm_reset: the later of the tpm/itpm reset of this entity and the otpm
-- one, or the otpm reset alone (memory.go otpmReset).
local function otpm_reset(tpm, itpm, ST, r)
  if tpm > 0 or itpm > 0 then return maxn(ST[6], r) end
  return r
end

-- peek_units fills ST with the token units evaluated after the denying unit
-- (rank: 0 none, rpm 1, tpm 2, itpm 3), reading without consuming
-- (memory.go peekTokens): a 429 by rpm still reports the token headers.
-- W() returns the win map.
local function peek_units(tb, W, now, after, tpm, itpm, otpm, fixed, ST)
  if tpm > 0 and after < 2 then
    if fixed then
      local s, c = fwin(W(), 'tpm', now, MIN)
      ST[4], ST[5], ST[6] = tpm, fl(maxn(0, tpm - c)), MIN - (now - s)
    else
      local bk = bucket(tb, 'tpm', now, tpm, tpm / 60)
      ST[4], ST[5], ST[6] = tpm, fl(maxn(0, bk.t)), fl(reset_ms(bk))
    end
  end
  if itpm > 0 and after < 3 then
    local bk = bucket(tb, 'itpm', now, itpm, itpm / 60)
    ST[7], ST[8], ST[6] = itpm, fl(maxn(0, bk.t)), fl(reset_ms(bk))
  end
  if otpm > 0 then
    local bk = bucket(tb, 'otpm', now, otpm, otpm / 60)
    ST[9], ST[10], ST[6] = otpm, fl(maxn(0, bk.t)), fl(otpm_reset(tpm, itpm, ST, reset_ms(bk)))
  end
end

-- deny builds the evaluation failure. st: 'acc' (accumulated state), a table
-- of 10 state numbers, or nil (no state).
local function deny(limit, retry, st, extra)
  return { limit = limit, retry = fl(maxn(0, retry or 0)), queue = 0, reason = '', st = st, extra = extra or 0 }
end

-- eval evaluates one entity: k = 7 keys (ent, tb, win, q5h, qwk, ramp, conc),
-- A = ARGV, b = index of LIM[1] in A. ST is the shared state (10 numbers:
-- req limit/remaining/reset, tok limit/remaining/reset, in limit/remaining,
-- out limit/remaining). Returns ok, failure, plan. Nothing is written here.
local function eval(k, A, b, now, ep, eo, cached, ms, ST)
  local L = {}
  for i = 0, 24 do L[i + 1] = A[b + i] end
  local mode, aftern, algo = n(L[3]), n(L[4]), n(L[5])
  local rps, burst, rpm, tpm, itpm, otpm, rpd, tpd = n(L[6]), n(L[7]), n(L[8]), n(L[9]), n(L[10]), n(L[11]), n(L[12]), n(L[13])
  local conc, qto, spendlim, credlim = n(L[14]), n(L[15]), n(L[16]), n(L[17])
  local qmo, q5pct, qwpct = n(L[18]), n(L[19]), n(L[20])
  local rth, rpct, rwin, excl, mask = n(L[21]), n(L[22]), n(L[23]), n(L[24]), n(L[25])
  if burst <= 0 then burst = rps end
  if rwin <= 0 then rwin = 900000 end
  local fixed = algo == 1

  local p = { k = k, kind = L[1], id = L[2], b = {}, w = {}, taken = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }, rps = rps, burst = burst, reserve = 0 }
  local e = redis.call('HMGET', k[1], 'kind', 'count', 'ban_since', 'ban_reason', 'blk_since', 'blk_reason', 'blk_until', 'credits')
  p.created = (e[1] == false)
  p.count = n(e[2]) + 1

  if e[3] then
    local f = deny('ban', 0, nil)
    f.reason = e[4] or ''
    return false, f, p
  end
  if e[5] then
    local u = n(e[7])
    if u == 0 or now < u then
      local ra = 0
      if u > 0 then ra = u - now end
      local f = deny('block', ra, nil)
      f.reason = e[6] or ''
      return false, f, p
    end
  end
  if mode == 2 or (mode == 1 and p.count <= aftern) then
    p.skip = true
    return true, nil, p
  end

  local tb = hmap(k[2])
  local win = nil
  local function W()
    if win == nil then win = hmap(k[3]) end
    return win
  end
  local function peek(after) peek_units(tb, W, now, after, tpm, itpm, otpm, fixed, ST) end

  -- rps
  if rps > 0 and mask % 2 == 0 then
    local bk = bucket(tb, 'rps', now, burst, rps)
    if bk.t >= 1 then
      bk.t = bk.t - 1
      p.b.rps, p.taken[1] = bk, 1
    else
      local r = reset_ms(bk)
      return false, deny('rps', maxn(r, 100), { rps, fl(maxn(0, bk.t)), fl(r), 0, 0, 0, 0, 0, 0, 0 }), p
    end
  end
  -- rpm
  if rpm > 0 and fl(mask / 2) % 2 == 0 then
    if fixed then
      local s, c = fwin(W(), 'rpm', now, MIN)
      c = c + 1
      local r = MIN - (now - s)
      ST[1], ST[2], ST[3] = rpm, fl(maxn(0, rpm - c)), r
      if c > rpm then
        peek(1)
        return false, deny('rpm', r, 'acc'), p
      end
      p.w.rpm, p.taken[5] = { s, c }, 1
    else
      local bk = bucket(tb, 'rpm', now, rpm, rpm / 60)
      local ok = bk.t >= 1
      if ok then bk.t = bk.t - 1 end
      local r = reset_ms(bk)
      ST[1], ST[2], ST[3] = rpm, fl(maxn(0, bk.t)), fl(r)
      if not ok then
        peek(1)
        return false, deny('rpm', maxn(60000 / rpm, 1000), 'acc'), p
      end
      p.b.rpm, p.taken[2] = bk, 1
    end
  end
  -- tpm (reserve prompt + output estimate)
  local reserve = ep + eo
  if excl == 1 then reserve = maxn(0, reserve - cached) end
  p.reserve = reserve
  if tpm > 0 then
    if fixed then
      local s, c = fwin(W(), 'tpm', now, MIN)
      c = c + reserve
      local r = MIN - (now - s)
      ST[4], ST[5], ST[6] = tpm, fl(maxn(0, tpm - c)), r
      if c > tpm then
        peek(2)
        return false, deny('tpm', r, 'acc'), p
      end
      p.w.tpm, p.taken[6] = { s, c }, reserve
    else
      local bk = bucket(tb, 'tpm', now, tpm, tpm / 60)
      local ok = bk.t >= reserve
      if ok then bk.t = bk.t - reserve end
      local rem = fl(maxn(0, bk.t))
      local r = reset_ms(bk)
      ST[4], ST[5], ST[6] = tpm, rem, fl(r)
      if not ok then
        peek(2)
        return false, deny('tpm', maxn((reserve - bk.t) / bk.rate * 1000, 100), 'acc', rem), p
      end
      p.b.tpm, p.taken[3] = bk, reserve
    end
  end
  -- itpm
  if itpm > 0 then
    local inp = ep
    if excl == 1 then inp = inp - cached end
    local bk = bucket(tb, 'itpm', now, itpm, itpm / 60)
    local ok = bk.t >= inp
    if ok then bk.t = bk.t - inp end
    local r = reset_ms(bk)
    ST[7], ST[8], ST[6] = itpm, fl(maxn(0, bk.t)), fl(r)
    if not ok then
      peek(3)
      return false, deny('itpm', maxn((inp - bk.t) / bk.rate * 1000, 100), 'acc'), p
    end
    p.b.itpm, p.taken[4] = bk, inp
  end
  -- otpm: headroom only; output is taken in finish.
  if otpm > 0 then
    local bk = bucket(tb, 'otpm', now, otpm, otpm / 60)
    ST[9], ST[10], ST[6] = otpm, fl(bk.t), fl(otpm_reset(tpm, itpm, ST, reset_ms(bk)))
    if bk.t < 1 then return false, deny('otpm', 1000, 'acc'), p end
    p.b.otpm = bk
  end
  -- daily windows (retry computed by the client: until next UTC midnight)
  if rpd > 0 then
    local s, c = fwin(W(), 'rpd', now, DAY)
    c = c + 1
    if c > rpd then return false, deny('rpd', 0, nil), p end
    p.w.rpd, p.taken[7] = { s, c }, 1
  end
  if tpd > 0 then
    local s, c = fwin(W(), 'tpd', now, DAY)
    c = c + reserve
    if c > tpd then return false, deny('tpd', 0, nil), p end
    p.w.tpd, p.taken[8] = { s, c }, reserve
  end
  -- spend / credits / quota
  if spendlim > 0 then
    local _, c = mwin(W(), 'spend', ms)
    if c >= spendlim then return false, deny('spend_monthly', 0, nil), p end
  end
  if credlim >= 0 then
    local cr = tonumber(e[8])
    if cr == nil then
      cr = credlim
      p.credits_init = credlim
    end
    if cr <= 0 then return false, deny('credits', 0, nil), p end
  end
  if qmo > 0 then
    local s5, o5, d5 = slide(hmap(k[4]), now, 5 * HOUR, MIN)
    p.q5del = d5
    if q5pct > 0 and s5 >= qmo * q5pct / 100 then
      return false, deny('quota_5h', (o5 or now) + 5 * HOUR - now, nil), p
    end
    -- The week turns on Monday 00:00 UTC (the epoch is a Thursday, so
    -- Mondays are 4 days after it modulo 7 days), as the real reports it.
    local wk = now - (now - 4 * DAY) % (7 * DAY)
    local sw, dw = since(hmap(k[5]), wk)
    p.qwdel = dw
    if qwpct > 0 and sw >= qmo * qwpct / 100 then
      return false, deny('quota_weekly', wk + 7 * DAY - now, nil), p
    end
    local _, cm = mwin(W(), 'qmo', ms)
    if cm >= qmo then return false, deny('quota_monthly', 0, nil), p end
  end
  -- ramp (acceleration limit)
  if rth > 0 and rpct > 0 then
    local prev, _, dr = slide(hmap(k[6]), now, rwin, MIN)
    local cur = prev + reserve
    if cur > rth and prev > 0 and cur > prev * (1 + rpct / 100) then
      return false, deny('ramp', rwin / 4, nil), p
    end
    p.rampdel, p.ramp, p.rwin, p.taken[9] = dr, reserve, rwin, reserve
  end
  -- concurrency
  if conc > 0 then
    local c = n(redis.call('GET', k[7]))
    if c >= conc then
      if qto > 0 then
        local f = deny('concurrency', qto, nil)
        f.queue = 1
        return false, f, p
      end
      return false, deny('concurrency', 1000, nil), p
    end
    p.conc, p.taken[10] = 1, 1
  end
  return true, nil, p
end

-- write_count persists what a denial keeps: the evaluation counter (and the
-- identity of a new entity).
local function write_count(p, now)
  local k = p.k[1]
  redis.call('HSET', k, 'count', p.count)
  if p.created then redis.call('HSET', k, 'kind', p.kind, 'id', p.id, 'since', now, 'status', 'active') end
  ent_ttl(k, now)
end

-- commit writes everything an admitted entity consumed.
local function commit(p, now, requests)
  local k = p.k
  write_count(p, now)
  if p.credits_init then
    redis.call('HSET', k[1], 'credits', p.credits_init)
    redis.call('PERSIST', k[1])
  end
  if requests then redis.call('HINCRBY', k[1], 'requests', 1) end
  if p.skip then return end
  local tbw = false
  for u, bk in pairs(p.b) do
    redis.call('HSET', k[2], u .. ':t', f6(bk.t), u .. ':ts', bk.ts, u .. ':cap', fg(bk.cap), u .. ':rate', fg(bk.rate))
    tbw = true
  end
  if tbw then
    local ttl = 120000
    if p.rps > 0 then ttl = maxn(ttl, fl(2 * p.burst / p.rps * 1000)) end
    redis.call('PEXPIRE', k[2], ttl)
  end
  local ww = false
  for w, sc in pairs(p.w) do
    redis.call('HSET', k[3], w .. ':s', sc[1], w .. ':c', sc[2])
    ww = true
  end
  if ww then redis.call('PEXPIRE', k[3], WIN_TTL) end
  if p.q5del and #p.q5del > 0 then redis.call('HDEL', k[4], unpack(p.q5del)) end
  if p.qwdel and #p.qwdel > 0 then redis.call('HDEL', k[5], unpack(p.qwdel)) end
  if p.ramp then
    if #p.rampdel > 0 then redis.call('HDEL', k[6], unpack(p.rampdel)) end
    redis.call('HINCRBY', k[6], fl(now / MIN) * MIN, p.ramp)
    redis.call('PEXPIRE', k[6], fl(p.rwin + MIN))
  end
  if p.conc then
    redis.call('INCR', k[7])
    redis.call('PEXPIRE', k[7], 120000)
  end
end

-- st_merge folds the state S of one entity into the request state ST: per
-- unit the entity with the smallest remaining, the later one on a tie
-- (valkey.go mergeState, memory.go mergeUnits; ADR 0051).
local function st_merge(ST, S)
  local function take(i) return S[i] > 0 and (ST[i] == 0 or S[i + 1] <= ST[i + 1]) end
  if take(1) then ST[1], ST[2], ST[3] = S[1], S[2], S[3] end
  local tok = false
  for _, i in ipairs({ 4, 7, 9 }) do
    if take(i) then ST[i], ST[i + 1], tok = S[i], S[i + 1], true end
  end
  if tok then ST[6] = S[6] end
end

-- st_project keeps in P the tokens unit of S when the entity is a project
-- with tpm (x-ratelimit-*-project-tokens of the openai driver; task 234).
-- Replies carry P as items 30-32 (valkey.go parseCheck).
local function st_project(P, kind, S)
  if kind == 'project' and S[4] > 0 then P[1], P[2], P[3] = S[4], S[5], S[6] end
end

-- st_tail appends the 10 taken items (T, or zeros) and the 3 project items.
local function st_tail(r, T, P)
  for i = 1, 10 do
    if T then r[19 + i] = fl(T[i]) else r[19 + i] = 0 end
  end
  for i = 1, 3 do r[29 + i] = fl(P[i]) end
end

-- st_reply expands a failure state into the 10 reply numbers.
local function st_reply(f, ST)
  local s = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }
  if f == nil or f.st == 'acc' then
    s = ST
  elseif type(f.st) == 'table' then
    s = f.st
  end
  local out = {}
  for i = 1, 10 do out[i] = fl(s[i]) end
  return out
end
