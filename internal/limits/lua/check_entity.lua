-- check_entity: evaluation of a single entity (cluster). KEYS: 7 keys.
-- ARGV: now, est_prompt, est_output, cached, month_start, next_month, then
-- the 25 LIM values. Reply: the 19 items of check plus 10 "taken" items
-- and the 3 project-tokens items (st_tail).
local now, ep, eo, cached, ms = n(ARGV[1]), n(ARGV[2]), n(ARGV[3]), n(ARGV[4]), n(ARGV[5])
local ST = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }
local ok, f, p = eval(KEYS, ARGV, 7, now, ep, eo, cached, ms, ST)
local P = { 0, 0, 0 }
st_project(P, ARGV[7], ST)
local created = 0
if p.created then created = 1 end
local r
if not ok then
  write_count(p, now)
  r = { 0, 1, f.limit, f.retry, f.queue }
  for _, v in ipairs(st_reply(f, ST)) do r[#r + 1] = v end
  r[16], r[17], r[18], r[19] = f.reason, created, fl(p.reserve), fl(f.extra)
  st_tail(r, nil, P)
  return r
end
commit(p, now, true)
r = { 1, 0, '', 0, 0 }
for _, v in ipairs(st_reply(nil, ST)) do r[#r + 1] = v end
r[16], r[17], r[18], r[19] = '', created, fl(p.reserve), 0
st_tail(r, p.taken, P)
return r
