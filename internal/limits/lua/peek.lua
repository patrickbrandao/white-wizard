-- peek: read-only token state of one entity (local_first rpm denial, task
-- 064). KEYS: tb, win. ARGV: now, tpm, itpm, otpm, algorithm (1 = fixed
-- window). Reply: the 10 state numbers (requests unit always 0).
local now, tpm, itpm, otpm = n(ARGV[1]), n(ARGV[2]), n(ARGV[3]), n(ARGV[4])
local tb = hmap(KEYS[1])
local win = nil
local function W()
  if win == nil then win = hmap(KEYS[2]) end
  return win
end
local ST = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 }
peek_units(tb, W, now, 0, tpm, itpm, otpm, n(ARGV[5]) == 1, ST)
local out = {}
for i = 1, 10 do out[i] = fl(ST[i]) end
return out
