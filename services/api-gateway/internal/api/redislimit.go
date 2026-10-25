package api

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindow is Limiter's algorithm in one atomic Redis call. State per key is one small hash
// (window index, current and previous count) that expires after two windows. Time comes from
// Redis itself so gateway instances with skewed clocks still agree.
// KEYS[1] key; ARGV[1] limit; ARGV[2] window in ms. Returns {allowed, retry_after_ms}.
var slidingWindow = redis.NewScript(`
local t = redis.call('TIME')
local ms = t[1] * 1000 + math.floor(t[2] / 1000)
local win = tonumber(ARGV[2])
local idx = math.floor(ms / win)
local frac = (ms % win) / win

local h = redis.call('HMGET', KEYS[1], 'w', 'c', 'p')
local w, c, p = tonumber(h[1]), tonumber(h[2]) or 0, tonumber(h[3]) or 0
if w == nil or idx - w >= 2 then
  c, p = 0, 0
elseif idx - w == 1 then
  p, c = c, 0
end

if p * (1 - frac) + c + 1 > tonumber(ARGV[1]) then
  return {0, win - (ms % win)}
end
redis.call('HSET', KEYS[1], 'w', idx, 'c', c + 1, 'p', p)
redis.call('PEXPIRE', KEYS[1], win * 2)
return {1, 0}
`)

const (
	redisTimeout  = 50 * time.Millisecond // a limiter check must never slow a request noticeably
	redisCooldown = time.Second           // after a Redis error, go straight to the fallback for this long
	redisLogEvery = 10 * time.Second
)

// RedisLimiter shares one budget per key across gateway instances. It fails open: when Redis
// errors or is slow, the request is judged by Fallback (a per-instance Limiter), so Redis being
// down costs accuracy across instances, never availability — and never leaves the gateway
// unprotected. After an error Redis is skipped for redisCooldown, so an outage adds no per-request latency.
type RedisLimiter struct {
	Client   redis.Scripter
	Prefix   string // namespaces keys, e.g. "ratelimit:ip:"
	Limit    int
	Window   time.Duration
	Fallback RateLimiter

	skipUntil atomic.Int64 // unix nanos
	lastLog   atomic.Int64
}

func (l *RedisLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()
	if now.UnixNano() < l.skipUntil.Load() {
		return l.Fallback.Allow(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	res, err := slidingWindow.Run(ctx, l.Client, []string{l.Prefix + key}, l.Limit, l.Window.Milliseconds()).Int64Slice()
	if err != nil || len(res) != 2 {
		l.skipUntil.Store(now.Add(redisCooldown).UnixNano())
		if last := l.lastLog.Load(); now.UnixNano()-last > int64(redisLogEvery) && l.lastLog.CompareAndSwap(last, now.UnixNano()) {
			log.Printf("rate limit: redis unavailable, using in-memory fallback: %v", err)
		}
		return l.Fallback.Allow(key)
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond
}
