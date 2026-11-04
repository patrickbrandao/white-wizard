-- check: atomic evaluation of every entity of a request (standalone).
-- KEYS: 7 keys per entity. ARGV: now, est_prompt, est_output, cached,
-- month_start, next_month, n, then 25 LIM values per entity. Reply: the 19
-- items, 10 zero "taken" items and the 3 project-tokens items (st_tail).
local now, ep, eo, cached, ms = n(ARGV[1]), n(ARGV[2]), n(ARGV[3]), n(ARGV[4]), n(ARGV[5])
local cnt = n(ARGV[7])
local ST = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }
local P = { 0, 0, 0 }
local plans = {}
for i = 1, cnt do
  local k = {}
  for j = 1, 7 do k[j] = KEYS[(i - 1) * 7 + j] end
  local S = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }
  local ok, f, p = eval(k, ARGV, 8 + 25 * (i - 1), now, ep, eo, cached, ms, S)
  st_merge(ST, S)
  st_project(P, ARGV[8 + 25 * (i - 1)], S)
  plans[i] = p
  if not ok then
    local created = 0
    for j = 1, i do
      write_count(plans[j], now)
      if plans[j].created then created = created + 2 ^ (j - 1) end
    end
    local r = { 0, i, f.limit, f.retry, f.queue }
    for _, v in ipairs(st_reply(f, ST)) do r[#r + 1] = v end
    r[16], r[17], r[18], r[19] = f.reason, created, fl(p.reserve), fl(f.extra)
    st_tail(r, nil, P)
    return r
  end
end
local created, reserve = 0, 0
for i = 1, cnt do
  commit(plans[i], now, true)
  if plans[i].created then created = created + 2 ^ (i - 1) end
  reserve = plans[i].reserve
end
local r = { 1, 0, '', 0, 0 }
for _, v in ipairs(st_reply(nil, ST)) do r[#r + 1] = v end
r[16], r[17], r[18], r[19] = '', created, fl(reserve), 0
st_tail(r, nil, P)
return r
