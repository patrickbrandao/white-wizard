-- set_status: Block, Unblock, Ban, Unban. Blocks and bans are independent
-- marks (blk_* and ban_* fields of ent), as in memory.go.
-- KEYS: ent. ARGV: op, now, reason, by, until_ms, kind, id.
-- Reply: {changed, previous status, created, new status}.
local op, now = ARGV[1], n(ARGV[2])
local key = KEYS[1]
local created = 0
if redis.call('EXISTS', key) == 0 then
  redis.call('HSET', key, 'kind', ARGV[6], 'id', ARGV[7], 'since', now, 'status', 'active')
  created = 1
end
local prev = status_of(key, now)
local changed = 0
if op == 'block' then
  redis.call('HSET', key, 'blk_since', now, 'blk_reason', ARGV[3], 'blk_by', ARGV[4], 'blk_until', n(ARGV[5]))
  changed = 1
elseif op == 'ban' then
  redis.call('HSET', key, 'ban_since', now, 'ban_reason', ARGV[3], 'ban_by', ARGV[4])
  changed = 1
elseif op == 'unblock' then
  local f = redis.call('HMGET', key, 'blk_since', 'blk_until')
  if f[1] then
    local u = n(f[2])
    if u == 0 or now < u then changed = 1 end
    redis.call('HDEL', key, 'blk_since', 'blk_reason', 'blk_by', 'blk_until')
  end
elseif op == 'unban' then
  if redis.call('HEXISTS', key, 'ban_since') == 1 then
    changed = 1
    redis.call('HDEL', key, 'ban_since', 'ban_reason', 'ban_by')
  end
end
local cur = status_of(key, now)
redis.call('HSET', key, 'status', cur)
ent_ttl(key, now)
return { changed, prev, created, cur }
