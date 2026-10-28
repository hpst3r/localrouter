package policy

import (
	"container/list"
	"strings"
	"time"
)

// affinityCap bounds the session-affinity table.
const affinityCap = 4096

// affinity is the bounded LRU of pooled-route session bindings. Keys are
// route name + "\x00" + the truncated X-LocalRouter-Session label; values are
// the bound member and the binding's expiry. It holds no request content. It
// is shared by every configuration view like Policy.state and guarded by
// Policy.mu.
type affinity struct {
	ll    *list.List // front = most recently used
	items map[string]*list.Element
}

type affinityEntry struct {
	key     string
	id      string
	expires time.Time
}

func newAffinity() *affinity {
	return &affinity{ll: list.New(), items: map[string]*list.Element{}}
}

// get returns the live binding for key; an expired binding reads as absent
// (put overwrites it, LRU order evicts it). It never mutates the table, so
// DryRunBalanced may call it.
func (a *affinity) get(key string, now time.Time) (string, bool) {
	el, ok := a.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*affinityEntry)
	if !now.Before(e.expires) {
		return "", false
	}
	return e.id, true
}

// put binds key to id until expires (sliding TTL) and evicts the least
// recently used binding beyond affinityCap.
func (a *affinity) put(key, id string, expires time.Time) {
	if el, ok := a.items[key]; ok {
		e := el.Value.(*affinityEntry)
		e.id, e.expires = id, expires
		a.ll.MoveToFront(el)
		return
	}
	a.items[key] = a.ll.PushFront(&affinityEntry{key: key, id: id, expires: expires})
	for a.ll.Len() > affinityCap {
		old := a.ll.Back()
		a.ll.Remove(old)
		delete(a.items, old.Value.(*affinityEntry).key)
	}
}

// count returns the number of live bindings, optionally for one route.
func (a *affinity) count(route string, now time.Time) int {
	n := 0
	for el := a.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*affinityEntry)
		if now.Before(e.expires) && (route == "" || routeOf(e.key) == route) {
			n++
		}
	}
	return n
}

func affinityKey(route, session string) string { return route + "\x00" + session }

func routeOf(key string) string {
	route, _, _ := strings.Cut(key, "\x00")
	return route
}
